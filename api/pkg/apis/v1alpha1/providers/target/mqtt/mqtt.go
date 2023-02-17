/*
   MIT License

   Copyright (c) Microsoft Corporation.

   Permission is hereby granted, free of charge, to any person obtaining a copy
   of this software and associated documentation files (the "Software"), to deal
   in the Software without restriction, including without limitation the rights
   to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
   copies of the Software, and to permit persons to whom the Software is
   furnished to do so, subject to the following conditions:

   The above copyright notice and this permission notice shall be included in all
   copies or substantial portions of the Software.

   THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
   IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
   FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
   AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
   LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
   OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
   SOFTWARE

*/

package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/azure/symphony/coa/pkg/apis/v1alpha2"
	"github.com/azure/symphony/coa/pkg/apis/v1alpha2/observability"
	observ_utils "github.com/azure/symphony/coa/pkg/apis/v1alpha2/observability/utils"
	"github.com/azure/symphony/coa/pkg/apis/v1alpha2/providers"
	"github.com/azure/symphony/coa/pkg/logger"
	"github.com/azure/symphony/api/pkg/apis/v1alpha1/model"
	gmqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"
)

var sLog = logger.NewLogger("coa.runtime")

type MQTTTargetProviderConfig struct {
	Name          string `json:"name"`
	BrokerAddress string `json:"brokerAddress"`
	ClientID      string `json:"clientID"`
	RequestTopic  string `json:"requestTopic"`
	ResponseTopic string `json:"responseTopic"`
}

var lock sync.Mutex

type ProxyResponse struct {
	IsOK    bool
	State   v1alpha2.State
	Payload interface{}
}
type MQTTTargetProvider struct {
	Config          MQTTTargetProviderConfig
	MQTTClient      gmqtt.Client
	GetChan         chan ProxyResponse
	RemoveChan      chan ProxyResponse
	NeedsUpdateChan chan ProxyResponse
	NeedsRemoveChan chan ProxyResponse
	ApplyChan       chan ProxyResponse
	Initialized     bool
}

func MQTTTargetProviderConfigFromMap(properties map[string]string) (MQTTTargetProviderConfig, error) {
	ret := MQTTTargetProviderConfig{}
	if v, ok := properties["name"]; ok {
		ret.Name = v
	}
	if v, ok := properties["brokerAddress"]; ok {
		ret.BrokerAddress = v
	} else {
		return ret, v1alpha2.NewCOAError(nil, "'brokerAdress' is missing in MQTT provider config", v1alpha2.BadConfig)
	}
	if v, ok := properties["clientID"]; ok {
		ret.ClientID = v
	} else {
		return ret, v1alpha2.NewCOAError(nil, "'clientID' is missing in MQTT provider config", v1alpha2.BadConfig)
	}
	if v, ok := properties["requestTopic"]; ok {
		ret.RequestTopic = v
	} else {
		return ret, v1alpha2.NewCOAError(nil, "'requestTopic' is missing in MQTT provider config", v1alpha2.BadConfig)
	}
	if v, ok := properties["responseTopic"]; ok {
		ret.ResponseTopic = v
	} else {
		return ret, v1alpha2.NewCOAError(nil, "'responseTopic' is missing in MQTT provider config", v1alpha2.BadConfig)
	}
	return ret, nil
}

func (i *MQTTTargetProvider) InitWithMap(properties map[string]string) error {
	config, err := MQTTTargetProviderConfigFromMap(properties)
	if err != nil {
		return err
	}
	return i.Init(config)
}

func (i *MQTTTargetProvider) Init(config providers.IProviderConfig) error {
	lock.Lock()
	defer lock.Unlock()

	_, span := observability.StartSpan("MQTT Target Provider", context.Background(), &map[string]string{
		"method": "Init",
	})
	sLog.Info("  P (MQTT Target): Init()")

	if i.Initialized {
		return nil
	}
	updateConfig, err := toMQTTTargetProviderConfig(config)
	if err != nil {
		observ_utils.CloseSpanWithError(span, err)
		sLog.Errorf("  P (MQTT Target): expected HttpTargetProviderConfig: %+v", err)
		return err
	}
	i.Config = updateConfig
	id := uuid.New()
	opts := gmqtt.NewClientOptions().AddBroker(i.Config.BrokerAddress).SetClientID(id.String())
	opts.SetKeepAlive(2 * time.Second)
	opts.SetPingTimeout(1 * time.Second)
	opts.CleanSession = true
	i.MQTTClient = gmqtt.NewClient(opts)
	if token := i.MQTTClient.Connect(); token.Wait() && token.Error() != nil {
		observ_utils.CloseSpanWithError(span, err)
		sLog.Errorf("  P (MQTT Target): faild to connect to MQTT broker - %+v", err)
		return v1alpha2.NewCOAError(token.Error(), "failed to connect to MQTT broker", v1alpha2.InternalError)
	}

	i.GetChan = make(chan ProxyResponse)
	i.RemoveChan = make(chan ProxyResponse)
	i.NeedsUpdateChan = make(chan ProxyResponse)
	i.NeedsRemoveChan = make(chan ProxyResponse)
	i.ApplyChan = make(chan ProxyResponse)

	if token := i.MQTTClient.Subscribe(i.Config.ResponseTopic, 0, func(client gmqtt.Client, msg gmqtt.Message) {
		var response v1alpha2.COAResponse
		json.Unmarshal(msg.Payload(), &response)
		proxyResponse := ProxyResponse{
			IsOK:  response.State == v1alpha2.OK || response.State == v1alpha2.Accepted,
			State: response.State,
		}
		if !proxyResponse.IsOK {
			proxyResponse.Payload = string(response.Body)
		}
		switch response.Metadata["call-context"] {
		case "TargetProvider-Get":
			if proxyResponse.IsOK {
				var ret []model.ComponentSpec
				err := json.Unmarshal(response.Body, &ret)
				if err != nil {
					sLog.Errorf("  P (MQTT Target): faild to deserialize components from MQTT - %+v, %s", err.Error(), string(response.Body))
				}
				proxyResponse.Payload = ret
			}
			i.GetChan <- proxyResponse
		case "TargetProvider-Remove":
			i.RemoveChan <- proxyResponse
		case "TargetProvider-NeedsUpdate":
			i.NeedsUpdateChan <- proxyResponse
		case "TargetProvider-NeedsRemove":
			i.NeedsRemoveChan <- proxyResponse
		case "TargetProvider-Apply":
			i.ApplyChan <- proxyResponse
		}
	}); token.Wait() && token.Error() != nil {
		if token.Error().Error() != "subscription exists" {
			sLog.Errorf("  P (MQTT Target): faild to connect to subscribe to the response topic - %+v", token.Error())
			return v1alpha2.NewCOAError(token.Error(), "failed to subscribe to response topic", v1alpha2.InternalError)
		}
	}
	i.Initialized = true
	observ_utils.CloseSpanWithError(span, nil)
	return nil
}
func toMQTTTargetProviderConfig(config providers.IProviderConfig) (MQTTTargetProviderConfig, error) {
	ret := MQTTTargetProviderConfig{}
	data, err := json.Marshal(config)
	if err != nil {
		return ret, err
	}
	err = json.Unmarshal(data, &ret)
	return ret, err
}
func (i *MQTTTargetProvider) Get(ctx context.Context, deployment model.DeploymentSpec) ([]model.ComponentSpec, error) {
	_, span := observability.StartSpan("MQTT Target Provider", ctx, &map[string]string{
		"method": "Get",
	})
	sLog.Infof("  P (MQTT Target): getting artifacts: %s - %s", deployment.Instance.Scope, deployment.Instance.Name)

	data, _ := json.Marshal(deployment)
	request := v1alpha2.COARequest{
		Route:  "instances",
		Method: "GET",
		Body:   data,
		Metadata: map[string]string{
			"call-context": "TargetProvider-Get",
		},
	}
	data, _ = json.Marshal(request)

	if token := i.MQTTClient.Publish(i.Config.RequestTopic, 0, false, data); token.Wait() && token.Error() != nil {
		sLog.Infof("  P (MQTT Target): failed to getting artifacts - %s", token.Error())
		observ_utils.CloseSpanWithError(span, token.Error())
		return nil, token.Error()
	}

	observ_utils.CloseSpanWithError(span, nil)
	timeout := time.After(8 * time.Second)
	select {
	case resp := <-i.GetChan:
		if resp.IsOK {
			data, err := json.Marshal(resp.Payload)
			if err != nil {
				sLog.Infof("  P (MQTT Target): failed to serialize payload - %s - %s", err.Error(), fmt.Sprint(resp.Payload))
				return nil, v1alpha2.NewCOAError(nil, err.Error(), v1alpha2.InternalError)
			}
			var ret []model.ComponentSpec
			err = json.Unmarshal(data, &ret)
			if err != nil {
				sLog.Infof("  P (MQTT Target): failed to deserialize components - %s - %s", err.Error(), fmt.Sprint(data))
				return nil, v1alpha2.NewCOAError(nil, err.Error(), v1alpha2.InternalError)
			}
			return ret, nil
		} else {
			return nil, v1alpha2.NewCOAError(nil, fmt.Sprint(resp.Payload), resp.State)
		}
	case <-timeout:
		return nil, v1alpha2.NewCOAError(nil, "didn't get response to Get() call over MQTT", v1alpha2.InternalError)
	}
}
func (i *MQTTTargetProvider) Remove(ctx context.Context, deployment model.DeploymentSpec, currentRef []model.ComponentSpec) error {
	_, span := observability.StartSpan("MQTT Target Provider", ctx, &map[string]string{
		"method": "Remove",
	})
	sLog.Infof("  P (MQTT Target): deleting artifacts: %s - %s", deployment.Instance.Scope, deployment.Instance.Name)

	data, _ := json.Marshal(deployment)
	request := v1alpha2.COARequest{
		Route:  "instances",
		Method: "DELETE",
		Body:   data,
		Metadata: map[string]string{
			"call-context": "TargetProvider-Remove",
		},
	}
	data, _ = json.Marshal(request)

	if token := i.MQTTClient.Publish(i.Config.RequestTopic, 0, false, data); token.Wait() && token.Error() != nil {
		observ_utils.CloseSpanWithError(span, token.Error())
		return token.Error()
	}

	observ_utils.CloseSpanWithError(span, nil)

	timeout := time.After(8 * time.Second)
	select {
	case resp := <-i.RemoveChan:
		if resp.IsOK {
			return nil
		} else {
			return v1alpha2.NewCOAError(nil, fmt.Sprint(resp.Payload), resp.State)
		}
	case <-timeout:
		return v1alpha2.NewCOAError(nil, "didn't get response to Remove() call over MQTT", v1alpha2.InternalError)
	}
}
func (i *MQTTTargetProvider) NeedsUpdate(ctx context.Context, desired []model.ComponentSpec, current []model.ComponentSpec) bool {
	_, span := observability.StartSpan("MQTT Target Provider", ctx, &map[string]string{
		"method": "NeedsUpdate",
	})
	sLog.Infof("  P (MQTT Target Provider): NeedsUpdate")

	data, _ := json.Marshal(TwoComponentSlices{
		Current: current,
		Desired: desired,
	})
	request := v1alpha2.COARequest{
		Route:  "needsupdate",
		Method: "GET",
		Body:   data,
		Metadata: map[string]string{
			"call-context": "TargetProvider-NeedsUpdate",
		},
	}
	data, _ = json.Marshal(request)

	if token := i.MQTTClient.Publish(i.Config.RequestTopic, 0, false, data); token.Wait() && token.Error() != nil {
		observ_utils.CloseSpanWithError(span, token.Error())
		return false
	}

	observ_utils.CloseSpanWithError(span, nil)

	timeout := time.After(8 * time.Second)
	select {
	case resp := <-i.NeedsUpdateChan:
		if resp.IsOK {
			return true
		} else {
			return false
		}
	case <-timeout:
		return false
	}
}
func (i *MQTTTargetProvider) NeedsRemove(ctx context.Context, desired []model.ComponentSpec, current []model.ComponentSpec) bool {
	_, span := observability.StartSpan("MQTT Target Provider", ctx, &map[string]string{
		"method": "NeedsRemove",
	})
	sLog.Infof("  P (MQTT Target): NeedsRemove")

	data, _ := json.Marshal(TwoComponentSlices{
		Current: current,
		Desired: desired,
	})
	request := v1alpha2.COARequest{
		Route:  "needsremove",
		Method: "GET",
		Body:   data,
		Metadata: map[string]string{
			"call-context": "TargetProvider-NeedsRemove",
		},
	}
	data, _ = json.Marshal(request)

	if token := i.MQTTClient.Publish(i.Config.RequestTopic, 0, false, data); token.Wait() && token.Error() != nil {
		observ_utils.CloseSpanWithError(span, token.Error())
		return false
	}

	observ_utils.CloseSpanWithError(span, nil)

	timeout := time.After(8 * time.Second)
	select {
	case resp := <-i.NeedsRemoveChan:
		if resp.IsOK {
			return true
		} else {
			return false
		}
	case <-timeout:
		return false
	}
}

func (i *MQTTTargetProvider) Apply(ctx context.Context, deployment model.DeploymentSpec) error {
	_, span := observability.StartSpan("MQTT Target Provider", ctx, &map[string]string{
		"method": "Apply",
	})
	sLog.Infof("  P (MQTT Target): applying artifacts: %s - %s", deployment.Instance.Scope, deployment.Instance.Name)

	data, _ := json.Marshal(deployment)
	request := v1alpha2.COARequest{
		Route:  "instances",
		Method: "POST",
		Body:   data,
		Metadata: map[string]string{
			"call-context": "TargetProvider-Apply",
		},
	}
	data, _ = json.Marshal(request)

	if token := i.MQTTClient.Publish(i.Config.RequestTopic, 0, false, data); token.Wait() && token.Error() != nil {
		observ_utils.CloseSpanWithError(span, token.Error())
		return token.Error()
	}

	observ_utils.CloseSpanWithError(span, nil)

	timeout := time.After(8 * time.Second)
	select {
	case resp := <-i.ApplyChan:
		if resp.IsOK {
			return nil
		} else {
			return v1alpha2.NewCOAError(nil, fmt.Sprint(resp.Payload), resp.State)
		}
	case <-timeout:
		return v1alpha2.NewCOAError(nil, "didn't get response to Apply() call over MQTT", v1alpha2.InternalError)
	}
}

type TwoComponentSlices struct {
	Current []model.ComponentSpec `json:"current"`
	Desired []model.ComponentSpec `json:"desired"`
}
