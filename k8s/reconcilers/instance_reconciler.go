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

package reconcilers

import (
	"context"
	"fmt"
	"strconv"
	"time"

	symphonyv1 "gopls-workspace/apis/symphony.microsoft.com/v1"
	"gopls-workspace/constants"
	"gopls-workspace/utils"

	apimodel "github.com/azure/symphony/api/pkg/apis/v1alpha1/model"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	provisioningstates "gopls-workspace/utils/models"

	"github.com/azure/symphony/api/pkg/apis/v1alpha1/model"
	api_utils "github.com/azure/symphony/api/pkg/apis/v1alpha1/utils"
	"github.com/azure/symphony/coa/pkg/apis/v1alpha2"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// InstanceReconciler reconciles a Instance object
type InstanceReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=symphony.microsoft.com,resources=instances,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=symphony.microsoft.com,resources=instances/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=symphony.microsoft.com,resources=instances/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Instance object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *InstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	myFinalizerName := "instance.solution.symphony/finalizer"

	log := ctrllog.FromContext(ctx)
	log.Info("Reconcile Instance")

	// Get instance
	instance := &symphonyv1.Instance{}
	if err := r.Client.Get(ctx, req.NamespacedName, instance); err != nil {
		log.Error(err, "unable to fetch Instance object")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	err := r.Client.Status().Update(ctx, instance)
	if err != nil {
		log.Error(err, "unable to update Instance status")
		return ctrl.Result{}, err
	}

	if instance.Status.Properties == nil {
		instance.Status.Properties = make(map[string]string)
	}

	if instance.ObjectMeta.DeletionTimestamp.IsZero() { // update
		if !controllerutil.ContainsFinalizer(instance, myFinalizerName) {
			controllerutil.AddFinalizer(instance, myFinalizerName)
			if err := r.Client.Update(ctx, instance); err != nil {
				return ctrl.Result{}, err
			}
		}

		summary, err := api_utils.GetSummary("http://symphony-service:8080/v1alpha2/", "admin", "", instance.ObjectMeta.Name)
		if err != nil && !v1alpha2.IsNotFound(err) {
			uErr := r.updateInstanceStatusToReconciling(instance, err)
			if uErr != nil {
				return ctrl.Result{}, uErr
			}
			return ctrl.Result{}, err
		}

		generationMatch := true
		if v, err := strconv.ParseInt(summary.Generation, 10, 64); err == nil {
			generationMatch = v == instance.GetGeneration()
		}

		if generationMatch && time.Since(summary.Time) <= time.Duration(60)*time.Second { //TODO: this is 60 second interval. Make if configurable?
			err = r.updateInstanceStatus(instance, summary.Summary)
			if err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		} else {
			// Queue a job every 60s or when the generation is changed
			err = api_utils.QueueJob("http://symphony-service:8080/v1alpha2/", "admin", "", instance.ObjectMeta.Name, false, false)
			if err != nil {
				uErr := r.updateInstanceStatusToReconciling(instance, err)
				if uErr != nil {
					return ctrl.Result{}, uErr
				}
				return ctrl.Result{}, err
			}

			// Update status to Reconciling if there is a change on generation
			// If users uninstall a component manually without modifying manifest
			// files, jobs queued every 60s will catch the descrepdency and
			// re-deploy the uninstalled component. As users' behavior doesn't
			// trigger generation change, this behavior won't change the status
			// to reconciling.
			if !generationMatch {
				err = r.updateInstanceStatusToReconciling(instance, nil)
				if err != nil {
					return ctrl.Result{}, err
				}
			}

			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}

	} else { // remove
		if controllerutil.ContainsFinalizer(instance, myFinalizerName) {
			err = api_utils.QueueJob("http://symphony-service:8080/v1alpha2/", "admin", "", instance.ObjectMeta.Name, true, false)

			if err != nil {
				uErr := r.updateInstanceStatusToReconciling(instance, err)
				if uErr != nil {
					return ctrl.Result{}, uErr
				}
				return ctrl.Result{}, err
			}
			timeout := time.After(5 * time.Minute)
			ticker := time.Tick(10 * time.Second) //TODO: configurable? adjust based on provider SLA?
		loop:
			for {
				select {
				case <-timeout:
					// Timeout exceeded, assume deletion failed and proceed with finalization
					break loop
				case <-ticker:
					summary, err := api_utils.GetSummary("http://symphony-service:8080/v1alpha2/", "admin", "", instance.ObjectMeta.Name)
					if err == nil && summary.Summary.IsRemoval == true && summary.Summary.SuccessCount == summary.Summary.TargetCount {
						break loop
					}
				}
			}
			// NOTE: we assume the message backend provides at-least-once delivery so that the removal event will be eventually handled.
			// Until the corresponding provider can successfully carry out the removal job, the job event will remain available for the
			// provider to pick up.
			controllerutil.RemoveFinalizer(instance, myFinalizerName)
			if err := r.Client.Update(ctx, instance); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{}, nil
}
func (r *InstanceReconciler) ensureOperationState(instance *symphonyv1.Instance, provisioningState string) {
	instance.Status.ProvisioningStatus.Status = provisioningState
	instance.Status.ProvisioningStatus.OperationID = instance.ObjectMeta.Annotations[constants.AzureOperationKey]
}

// updateInstanceStatusToReconciling updates Instance object to Reconciling (non-terminal) state
func (r *InstanceReconciler) updateInstanceStatusToReconciling(instance *symphonyv1.Instance, err error) error {
	if instance.Status.Properties == nil {
		instance.Status.Properties = make(map[string]string)
	}
	instance.Status.Properties["status"] = provisioningstates.Reconciling
	instance.Status.Properties["deployed"] = "pending"
	instance.Status.Properties["targets"] = "pending"
	instance.Status.Properties["status-details"] = ""
	if err != nil {
		instance.Status.Properties["status-details"] = fmt.Sprintf("Reconciling due to %s", err.Error())
	}
	r.updateProvisioningStatusToReconciling(instance, err)
	instance.Status.LastModified = metav1.Now()
	return r.Client.Status().Update(context.Background(), instance)
}
func (r *InstanceReconciler) updateInstanceStatus(instance *symphonyv1.Instance, summary model.SummarySpec) error {
	if instance.Status.Properties == nil {
		instance.Status.Properties = make(map[string]string)
	}
	targetCount := strconv.Itoa(summary.TargetCount)
	successCount := strconv.Itoa(summary.SuccessCount)
	status := provisioningstates.Succeeded
	if successCount != targetCount {
		status = provisioningstates.Failed
	}
	instance.Status.Properties["status"] = status
	instance.Status.Properties["deployed"] = successCount
	instance.Status.Properties["targets"] = targetCount
	instance.Status.Properties["status-details"] = summary.SummaryMessage

	// If a component is ever deployed, it will always show in Status.Properties
	// If a component is not deleted, it will first be reset to Untouched and
	// then changed to corresponding status later
	for k, v := range instance.Status.Properties {
		if utils.IsComponentKey(k) && v != v1alpha2.Deleted.String() {
			instance.Status.Properties[k] = v1alpha2.Untouched.String()
		}
	}

	// Change to corresponding status
	for k, v := range summary.TargetResults {
		instance.Status.Properties["targets."+k] = fmt.Sprintf("%s - %s", v.Status, v.Message)
		for ck, cv := range v.ComponentResults {
			instance.Status.Properties["targets."+k+"."+ck] = fmt.Sprintf("%s - %s", cv.Status, cv.Message)
		}
	}

	r.updateProvisioningStatus(instance, status, summary)
	instance.Status.LastModified = metav1.Now()
	return r.Client.Status().Update(context.Background(), instance)
}

func (r *InstanceReconciler) updateProvisioningStatus(instance *symphonyv1.Instance, provisioningStatus string, summary model.SummarySpec) {
	r.ensureOperationState(instance, provisioningStatus)
	// Start with a clean Error object and update all the fields
	instance.Status.ProvisioningStatus.Error = apimodel.ErrorType{}
	// Output field is updated if status is Succeeded
	instance.Status.ProvisioningStatus.Output = make(map[string]string)

	if provisioningStatus == provisioningstates.Failed {
		errorObj := &instance.Status.ProvisioningStatus.Error

		// Fill error details into error object
		errorObj.Code = "Symphony: [500]"
		errorObj.Message = "Deployment failed."
		errorObj.Target = "Symphony"
		errorObj.Details = make([]apimodel.TargetError, 0)
		for k, v := range summary.TargetResults {
			targetObject := apimodel.TargetError{
				Code:    v.Status,
				Message: v.Message,
				Target:  k,
				Details: make([]apimodel.ComponentError, 0),
			}
			for ck, cv := range v.ComponentResults {
				targetObject.Details = append(targetObject.Details, apimodel.ComponentError{
					Code:    cv.Status.String(),
					Message: cv.Message,
					Target:  ck,
				})
			}
			errorObj.Details = append(errorObj.Details, targetObject)
		}
	} else if provisioningStatus == provisioningstates.Succeeded {
		outputMap := instance.Status.ProvisioningStatus.Output
		// Fill component details into output field
		for k, v := range summary.TargetResults {
			for ck, cv := range v.ComponentResults {
				outputMap[fmt.Sprintf("%s.%s", k, ck)] = cv.Status.String()
			}
		}
	}
}

// updateProvisioningStatusToReconciling updates ProvisioningStatus to Reconciling (non-terminal) state
func (r *InstanceReconciler) updateProvisioningStatusToReconciling(instance *symphonyv1.Instance, err error) {
	provisioningStatus := provisioningstates.Reconciling
	if err != nil {
		provisioningStatus = fmt.Sprintf("%s: due to %s", provisioningstates.Reconciling, err.Error())
	}
	r.ensureOperationState(instance, provisioningStatus)
	// Start with a clean Error object and update all the fields
	instance.Status.ProvisioningStatus.Error = apimodel.ErrorType{}
}
