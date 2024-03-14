/*
 * Copyright (c) Microsoft Corporation.
 * Licensed under the MIT license.
 * SPDX-License-Identifier: MIT
 */

package memorystate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2"
	contexts "github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/contexts"
	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/observability"
	observ_utils "github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/observability/utils"
	providers "github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/providers"
	states "github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/providers/states"
	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/utils"
	"github.com/eclipse-symphony/symphony/coa/pkg/logger"
)

var sLog = logger.NewLogger("coa.runtime")
var mLock sync.RWMutex

type MemoryStateProviderConfig struct {
	Name string `json:"name"`
}

func MemoryStateProviderConfigFromMap(properties map[string]string) (MemoryStateProviderConfig, error) {
	ret := MemoryStateProviderConfig{}
	if v, ok := properties["name"]; ok {
		ret.Name = utils.ParseProperty(v)
	}
	return ret, nil
}

type MemoryStateProvider struct {
	Config  MemoryStateProviderConfig
	Data    map[string]interface{}
	Context *contexts.ManagerContext
}

func (s *MemoryStateProvider) ID() string {
	return s.Config.Name
}

func (s *MemoryStateProvider) SetContext(ctx *contexts.ManagerContext) {
	s.Context = ctx
}

func (i *MemoryStateProvider) InitWithMap(properties map[string]string) error {
	config, err := MemoryStateProviderConfigFromMap(properties)
	if err != nil {
		return err
	}
	return i.Init(config)
}

func (s *MemoryStateProvider) Init(config providers.IProviderConfig) error {
	// parameter checks
	stateConfig, err := toMemoryStateProviderConfig(config)
	if err != nil {
		sLog.Errorf("  P (Memory State): failed to parse provider config %+v", err)
		return errors.New("expected MemoryStateProviderConfig")
	}
	s.Config = stateConfig
	s.Data = make(map[string]interface{}, 0)
	return nil
}

func (s *MemoryStateProvider) Upsert(ctx context.Context, entry states.UpsertRequest) (string, error) {
	mLock.Lock()
	defer mLock.Unlock()

	_, span := observability.StartSpan("Memory State Provider", ctx, &map[string]string{
		"method": "Upsert",
	})
	sLog.Debugf("  P (Memory State): upsert states %s, traceId: %s", entry.Value.ID, span.SpanContext().TraceID().String())

	var err error = nil
	defer observ_utils.CloseSpanWithError(span, &err)

	tag := "1"
	if entry.Value.ETag != "" {
		var v int64
		if v, err = strconv.ParseInt(entry.Value.ETag, 10, 64); err == nil {
			tag = strconv.FormatInt(v+1, 10)
		}
	}
	entry.Value.ETag = tag

	if entry.Options.UpdateStateOnly {
		existing, ok := s.Data[entry.Value.ID]
		if !ok {
			err = v1alpha2.NewCOAError(nil, fmt.Sprintf("entry '%s' is not found", entry.Value.ID), v1alpha2.NotFound)
			sLog.Errorf("  P (Memory State): failed to upsert %s state: %+v, traceId: %s", entry.Value.ID, err, span.SpanContext().TraceID().String())
			return "", err
		}
		existingEntry, ok := existing.(states.StateEntry)
		if !ok {
			err = v1alpha2.NewCOAError(nil, fmt.Sprintf("entry '%s' is not a valid state entry", entry.Value.ID), v1alpha2.InternalError)
			sLog.Errorf("  P (Memory State): failed to upsert %s state: %+v, traceId: %s", entry.Value.ID, err, span.SpanContext().TraceID().String())
			return "", err
		}

		mapRef := existingEntry.Body.(map[string]interface{})
		var mapType map[string]interface{}
		jBody, _ := json.Marshal(entry.Value.Body)
		json.Unmarshal(jBody, &mapType)

		if mapRef["status"] == nil {
			mapRef["status"] = make(map[string]interface{})
		}
		for k, v := range mapType["status"].(map[string]interface{}) {
			mapRef["status"].(map[string]interface{})[k] = v
		}

		entry.Value.Body = mapRef
	}

	s.Data[entry.Value.ID] = entry.Value

	return entry.Value.ID, nil
}
func traceDownField(entity map[string]interface{}, filter string) (map[string]interface{}, string, error) {
	if !strings.Contains(filter, ".") {
		return entity, filter, nil
	}
	parts := strings.Split(filter, ".")
	if v, ok := entity[parts[0]]; ok {
		var dict = make(map[string]interface{})
		j, _ := json.Marshal(v)
		err := json.Unmarshal(j, &dict)
		if err != nil {
			return nil, filter, err
		}
		return traceDownField(dict, strings.Join(parts[1:], "."))
	} else {
		return nil, filter, v1alpha2.NewCOAError(nil, fmt.Sprintf("filter '%s' is not a valid selector", filter), v1alpha2.BadRequest)
	}
}
func simulateK8sFilter(entity map[string]interface{}, filter string) (bool, error) {
	if strings.Index(filter, "!=") > 0 {
		parts := strings.Split(filter, "!=")
		if len(parts) == 2 {
			dict, key, err := traceDownField(entity, parts[0])
			if err != nil {
				return false, err
			}
			if dict[key] != nil {
				if dict[key] != parts[1] {
					return true, nil
				}
			}
			return false, nil
		} else {
			return false, v1alpha2.NewCOAError(nil, fmt.Sprintf("filter '%s' is not a valid selector", filter), v1alpha2.BadRequest)
		}
	} else if strings.Index(filter, "=") > 0 {
		parts := strings.Split(filter, "=")
		if len(parts) == 2 {
			dict, key, err := traceDownField(entity, parts[0])
			if err != nil {
				return false, err
			}
			if dict[key] != nil {
				if dict[key] == parts[1] {
					return true, nil
				}
			}
			return false, nil
		} else {
			return false, v1alpha2.NewCOAError(nil, fmt.Sprintf("filter '%s' is not a valid selector", filter), v1alpha2.BadRequest)
		}
	} else {
		return false, v1alpha2.NewCOAError(nil, fmt.Sprintf("filter '%s' is not a valid selector", filter), v1alpha2.BadRequest)
	}
}
func (s *MemoryStateProvider) List(ctx context.Context, request states.ListRequest) ([]states.StateEntry, string, error) {
	mLock.RLock()
	defer mLock.RUnlock()
	_, span := observability.StartSpan("Memory State Provider", ctx, &map[string]string{
		"method": "List",
	})
	var err error = nil
	defer observ_utils.CloseSpanWithError(span, &err)

	sLog.Debugf("  P (Memory State): list states, traceId: %s", span.SpanContext().TraceID().String())

	var entities []states.StateEntry
	for _, v := range s.Data {
		vE, ok := v.(states.StateEntry)
		if ok {
			if request.FilterType != "" && request.FilterValue != "" {
				var dict map[string]interface{}
				j, _ := json.Marshal(vE.Body)
				err = json.Unmarshal(j, &dict)
				if err != nil {
					err = v1alpha2.NewCOAError(nil, "failed to unmarshal state entry", v1alpha2.InternalError)
					sLog.Errorf("  P (Memory State): failed to list states: %+v, traceId: %s", err, span.SpanContext().TraceID().String())
					return entities, "", err
				}
				switch request.FilterType {
				case "label":
					if dict["metadata"] != nil {
						metadata, ok := dict["metadata"].(map[string]interface{})
						if ok {
							if metadata["labels"] != nil {
								labels, ok := metadata["labels"].(map[string]interface{})
								if ok {
									match, err := simulateK8sFilter(labels, request.FilterValue)
									if err != nil {
										return entities, "", err
									}
									if !match {
										continue
									}
								}
							}
						}
					}
				case "field":
					match, err := simulateK8sFilter(dict, request.FilterValue)
					if err != nil {
						return entities, "", err
					}
					if !match {
						continue
					}
				case "status":
					if dict["status"] != nil {
						status, ok := dict["status"].(map[string]interface{})
						if ok {
							if v, e := utils.JsonPathQuery(status, request.FilterValue); e != nil || v == nil {
								continue
							}
						}
					}
				case "spec":
					if dict["spec"] != nil {
						spec, ok := dict["spec"].(map[string]interface{})
						if ok {
							if v, e := utils.JsonPathQuery(spec, request.FilterValue); e != nil || v == nil {
								continue
							}
						}
					}
				}
			}
			entities = append(entities, vE)
		} else {
			err = v1alpha2.NewCOAError(nil, "found invalid state entry", v1alpha2.InternalError)
			sLog.Errorf("  P (Memory State): failed to list states: %+v, traceId: %s", err, span.SpanContext().TraceID().String())
			return entities, "", err
		}
	}

	return entities, "", nil
}

func (s *MemoryStateProvider) Delete(ctx context.Context, request states.DeleteRequest) error {
	mLock.Lock()
	defer mLock.Unlock()
	_, span := observability.StartSpan("Memory State Provider", ctx, &map[string]string{
		"method": "Delete",
	})
	var err error = nil
	defer observ_utils.CloseSpanWithError(span, &err)

	sLog.Debugf("  P (Memory State): delete state %s, traceId: %s", request.ID, span.SpanContext().TraceID().String())

	if _, ok := s.Data[request.ID]; !ok {
		err = v1alpha2.NewCOAError(nil, fmt.Sprintf("entry '%s' is not found", request.ID), v1alpha2.NotFound)
		sLog.Errorf("  P (Memory State): failed to delete %s: %+v, traceId: %s", request.ID, err, span.SpanContext().TraceID().String())
		return err
	}
	delete(s.Data, request.ID)

	return nil
}

func (s *MemoryStateProvider) Get(ctx context.Context, request states.GetRequest) (states.StateEntry, error) {
	mLock.RLock()
	defer mLock.RUnlock()
	_, span := observability.StartSpan("Memory State Provider", ctx, &map[string]string{
		"method": "Get",
	})
	var err error = nil
	defer observ_utils.CloseSpanWithError(span, &err)

	sLog.Debugf("  P (Memory State): get state %s, traceId: %s", request.ID, span.SpanContext().TraceID().String())

	if v, ok := s.Data[request.ID]; ok {
		vE, ok := v.(states.StateEntry)
		if ok {
			err = nil
			return vE, nil
		} else {
			err = v1alpha2.NewCOAError(nil, fmt.Sprintf("entry '%s' is not a valid state entry", request.ID), v1alpha2.InternalError)
			sLog.Errorf("  P (Memory State): failed to get %s state: %+v, traceId: %s", request.ID, err, span.SpanContext().TraceID().String())
			return states.StateEntry{}, err
		}
	}
	err = v1alpha2.NewCOAError(nil, fmt.Sprintf("entry '%s' is not found", request.ID), v1alpha2.NotFound)
	sLog.Errorf("  P (Memory State): failed to get %s state: %+v, traceId: %s", request.ID, err, span.SpanContext().TraceID().String())
	return states.StateEntry{}, err
}

func toMemoryStateProviderConfig(config providers.IProviderConfig) (MemoryStateProviderConfig, error) {
	ret := MemoryStateProviderConfig{}
	data, err := json.Marshal(config)
	if err != nil {
		return ret, err
	}
	err = json.Unmarshal(data, &ret)
	//ret.Name = providers.LoadEnv(ret.Name)
	return ret, err
}

func (a *MemoryStateProvider) Clone(config providers.IProviderConfig) (providers.IProvider, error) {
	ret := &MemoryStateProvider{}
	if config == nil {
		err := ret.Init(a.Config)
		if err != nil {
			return nil, err
		}
	} else {
		err := ret.Init(config)
		if err != nil {
			return nil, err
		}
	}
	if a.Context != nil {
		ret.Context = a.Context
	}
	return ret, nil
}
