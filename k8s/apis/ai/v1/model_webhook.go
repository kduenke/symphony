/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	configv1 "gopls-workspace/apis/config/v1"
	configutils "gopls-workspace/configutils"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// log is for logging in this package.
var modellog = logf.Log.WithName("model-resource")
var myModelClient client.Client
var modelValidationPolicies []configv1.ValidationPolicy

func (r *Model) SetupWebhookWithManager(mgr ctrl.Manager) error {
	myModelClient = mgr.GetClient()
	mgr.GetFieldIndexer().IndexField(context.Background(), &Model{}, ".spec.displayName", func(rawObj client.Object) []string {
		model := rawObj.(*Model)
		return []string{model.Spec.DisplayName}
	})

	dict, _ := configutils.GetValidationPoilicies()
	if v, ok := dict["model"]; ok {
		modelValidationPolicies = v
	}

	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

//+kubebuilder:webhook:path=/mutate-ai-symphony-v1-model,mutating=true,failurePolicy=fail,sideEffects=None,groups=ai.symphony,resources=models,verbs=create;update,versions=v1,name=mmodel.kb.io,admissionReviewVersions=v1

var _ webhook.Defaulter = &Model{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *Model) Default() {
	modellog.Info("default", "name", r.Name)

	if r.Spec.DisplayName == "" {
		r.Spec.DisplayName = r.ObjectMeta.Name
	}
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.

//+kubebuilder:webhook:path=/validate-ai-symphony-v1-model,mutating=false,failurePolicy=fail,sideEffects=None,groups=ai.symphony,resources=models,verbs=create;update,versions=v1,name=vmodel.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &Model{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *Model) ValidateCreate() error {
	modellog.Info("validate create", "name", r.Name)

	return r.validateCreateModel()
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *Model) ValidateUpdate(old runtime.Object) error {
	modellog.Info("validate update", "name", r.Name)

	return r.validateUpdateModel()
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *Model) ValidateDelete() error {
	modellog.Info("validate delete", "name", r.Name)

	// TODO(user): fill in your validation logic upon object deletion.
	return nil
}

func (r *Model) validateCreateModel() error {
	var allErrs field.ErrorList
	var models ModelList
	err := myModelClient.List(context.Background(), &models, client.InNamespace(r.Namespace), client.MatchingFields{".spec.displayName": r.Spec.DisplayName})
	if err != nil {
		allErrs = append(allErrs, field.InternalError(&field.Path{}, err))
		return apierrors.NewInvalid(schema.GroupKind{Group: "ai.symphony", Kind: "Model"}, r.Name, allErrs)
	}
	if len(models.Items) != 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec").Child("displayName"), r.Spec.DisplayName, "model display name is already taken"))
		return apierrors.NewInvalid(schema.GroupKind{Group: "ai.symphony", Kind: "Model"}, r.Name, allErrs)
	}
	if len(modelValidationPolicies) > 0 {
		err := myModelClient.List(context.Background(), &models, client.InNamespace(r.Namespace), &client.ListOptions{})
		if err != nil {
			allErrs = append(allErrs, field.InternalError(&field.Path{}, err))
			return apierrors.NewInvalid(schema.GroupKind{Group: "ai.symphony", Kind: "Model"}, r.Name, allErrs)
		}
		for _, p := range modelValidationPolicies {
			pack := extractModelValidationPack(models, p)
			ret, err := configutils.CheckValidationPack(r.ObjectMeta.Name, readModelValiationTarget(r, p), p.ValidationType, pack)
			if err != nil {
				return err
			}
			if ret != "" {
				allErrs = append(allErrs, field.Forbidden(&field.Path{}, strings.ReplaceAll(p.Message, "%s", ret)))
				return apierrors.NewInvalid(schema.GroupKind{Group: "ai.symphony", Kind: "Model"}, r.Name, allErrs)
			}
		}
	}
	return nil
}

func (r *Model) validateUpdateModel() error {
	var allErrs field.ErrorList
	var models ModelList
	err := myModelClient.List(context.Background(), &models, client.InNamespace(r.Namespace), client.MatchingFields{".spec.displayName": r.Spec.DisplayName})
	if err != nil {
		allErrs = append(allErrs, field.InternalError(&field.Path{}, err))
		return apierrors.NewInvalid(schema.GroupKind{Group: "ai.symphony", Kind: "Model"}, r.Name, allErrs)
	}
	if !(len(models.Items) == 0 || len(models.Items) == 1 && models.Items[0].ObjectMeta.Name == r.ObjectMeta.Name) {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec").Child("displayName"), r.Spec.DisplayName, "model display name is already taken"))
		return apierrors.NewInvalid(schema.GroupKind{Group: "ai.symphony", Kind: "Model"}, r.Name, allErrs)
	}
	if len(modelValidationPolicies) > 0 {
		err := myModelClient.List(context.Background(), &models, client.InNamespace(r.Namespace), &client.ListOptions{})
		if err != nil {
			allErrs = append(allErrs, field.InternalError(&field.Path{}, err))
			return apierrors.NewInvalid(schema.GroupKind{Group: "ai.symphony", Kind: "Model"}, r.Name, allErrs)
		}
		for _, p := range modelValidationPolicies {
			pack := extractModelValidationPack(models, p)
			ret, err := configutils.CheckValidationPack(r.ObjectMeta.Name, readModelValiationTarget(r, p), p.ValidationType, pack)
			if err != nil {
				return err
			}
			if ret != "" {
				allErrs = append(allErrs, field.Forbidden(&field.Path{}, strings.ReplaceAll(p.Message, "%s", ret)))
				return apierrors.NewInvalid(schema.GroupKind{Group: "ai.symphony", Kind: "Model"}, r.Name, allErrs)
			}
		}
	}
	return nil
}

func readModelValiationTarget(model *Model, p configv1.ValidationPolicy) string {
	if p.SelectorType == "properties" {
		if v, ok := model.Spec.Properties[p.SpecField]; ok {
			return v
		}
	}
	return ""
}

func extractModelValidationPack(list ModelList, p configv1.ValidationPolicy) []configv1.ValidationStruct {
	pack := make([]configv1.ValidationStruct, 0)
	for _, t := range list.Items {
		s := configv1.ValidationStruct{}
		if p.SelectorType == "properties" {
			if v, ok := t.Spec.Properties[p.SpecField]; ok {
				s.Field = v
				s.Name = t.ObjectMeta.Name
				pack = append(pack, s)
			}
		}
	}
	return pack
}
