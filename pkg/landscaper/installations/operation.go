// Copyright 2020 Copyright (c) 2020 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package installations

import (
	"context"
	"fmt"

	cdv2 "github.com/gardener/component-spec/bindings-go/apis/v2"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	componentsregistry "github.com/gardener/landscaper/pkg/landscaper/registry/components"
	"github.com/gardener/landscaper/pkg/landscaper/registry/components/cdutils"

	lsv1alpha1 "github.com/gardener/landscaper/pkg/apis/core/v1alpha1"
	lsv1alpha1helper "github.com/gardener/landscaper/pkg/apis/core/v1alpha1/helper"
	"github.com/gardener/landscaper/pkg/kubernetes"
	"github.com/gardener/landscaper/pkg/landscaper/datatype"
	lsoperation "github.com/gardener/landscaper/pkg/landscaper/operation"
	"github.com/gardener/landscaper/pkg/landscaper/registry/blueprints"
	"github.com/gardener/landscaper/pkg/utils"
	kubernetesutil "github.com/gardener/landscaper/pkg/utils/kubernetes"
)

// Operation contains all installation operations and implements the Operation interface.
type Operation struct {
	lsoperation.Interface
	Datatypes map[string]*datatype.Datatype

	Inst                        *Installation
	ComponentDescriptor         *cdv2.ComponentDescriptor
	ResolvedComponentDescriptor cdv2.ComponentDescriptorList
	context                     *Context
	staticData                  map[string]interface{}
}

// NewInstallationOperation creates a new installation operation
func NewInstallationOperation(ctx context.Context, log logr.Logger, c client.Client, scheme *runtime.Scheme, bRegistry blueprintsregistry.Registry, cRegistry componentsregistry.Registry, datatypes map[string]*datatype.Datatype, inst *Installation) (*Operation, error) {
	return NewInstallationOperationFromOperation(ctx, lsoperation.NewOperation(log, c, scheme, bRegistry, cRegistry), datatypes, inst)
}

// NewInstallationOperationFromOperation creates a new installation operation from an existing common operation
func NewInstallationOperationFromOperation(ctx context.Context, op lsoperation.Interface, datatypes map[string]*datatype.Datatype, inst *Installation) (*Operation, error) {
	instOp := &Operation{
		Interface: op,
		Datatypes: datatypes,
		Inst:      inst,
	}

	if err := instOp.ResolveComponentDescriptors(ctx); err != nil {
		return nil, err
	}

	var err error
	instOp.context, err = instOp.DetermineContext(ctx)
	if err != nil {
		return nil, err
	}

	return instOp, nil
}

// ResolveComponentDescriptors resolves the effective component descriptors for the installation.
func (o *Operation) ResolveComponentDescriptors(ctx context.Context) error {
	cd, err := ResolveComponentDescriptor(ctx, o.ComponentsRegistry(), o.Inst.Info)
	if err != nil {
		return err
	}

	resolvedCD, err := cdutils.ResolveEffectiveComponentDescriptorList(ctx, o.ComponentsRegistry(), *cd)
	if err != nil {
		return err
	}
	o.ComponentDescriptor = cd
	o.ResolvedComponentDescriptor = resolvedCD
	return nil
}

// Context returns the context of the operated installation
func (o *Operation) Context() *Context {
	return o.context
}

// GetDataType returns the datatype with a specific name.
// It returns ok = false if the datatype does not exist.
func (o *Operation) GetDataType(name string) (dt *datatype.Datatype, ok bool) {
	dt, ok = o.Datatypes[name]
	return
}

// UpdateInstallationStatus updates the status of a installation
func (o *Operation) UpdateInstallationStatus(ctx context.Context, inst *lsv1alpha1.Installation, phase lsv1alpha1.ComponentInstallationPhase, updatedConditions ...lsv1alpha1.Condition) error {
	inst.Status.Phase = phase
	inst.Status.Conditions = lsv1alpha1helper.MergeConditions(inst.Status.Conditions, updatedConditions...)
	if err := o.Client().Status().Update(ctx, inst); err != nil {
		o.Log().Error(err, "unable to set installation status")
		return err
	}
	return nil
}

// CreateEventFromCondition creates a new event based on the given condition
func (o *Operation) CreateEventFromCondition(ctx context.Context, inst *lsv1alpha1.Installation, cond lsv1alpha1.Condition) error {
	event := &corev1.Event{}
	event.GenerateName = "inst-"
	event.Namespace = inst.Namespace
	event.Type = "Warning"
	event.Source = corev1.EventSource{
		Component: "landscaper", // todo: make configurable by the caller
	}
	event.InvolvedObject = corev1.ObjectReference{
		Kind:            inst.Kind,
		Namespace:       inst.Namespace,
		Name:            inst.Name,
		UID:             inst.UID,
		APIVersion:      inst.APIVersion,
		ResourceVersion: inst.ResourceVersion,
	}
	event.Reason = cond.Reason
	event.Message = cond.Message
	event.Action = string(cond.Type)

	if err := o.Client().Create(ctx, event); err != nil {
		o.Log().Error(err, "unable to set installation status")
		return err
	}
	return nil
}

// GetRootInstallations returns all root installations in the system
func (o *Operation) GetRootInstallations(ctx context.Context, filter func(lsv1alpha1.Installation) bool, opts ...client.ListOption) ([]*lsv1alpha1.Installation, error) {
	r, err := labels.NewRequirement(lsv1alpha1.EncompassedByLabel, selection.DoesNotExist, nil)
	if err != nil {
		return nil, err
	}
	opts = append(opts, client.MatchingLabelsSelector{Selector: labels.NewSelector().Add(*r)})

	installationList := &lsv1alpha1.InstallationList{}
	if err := o.Client().List(ctx, installationList, opts...); err != nil {
		return nil, err
	}

	installations := make([]*lsv1alpha1.Installation, 0)
	for _, obj := range installationList.Items {
		if filter != nil && filter(obj) {
			continue
		}
		inst := obj
		installations = append(installations, &inst)
	}
	return installations, nil
}

// GetStaticData constructs the static data from the installation.
func (o *Operation) GetStaticData(ctx context.Context) (map[string]interface{}, error) {
	if o.staticData != nil {
		return o.staticData, nil
	}

	if len(o.Inst.Info.Spec.StaticData) == 0 {
		return nil, nil
	}

	data := make(map[string]interface{})
	for _, source := range o.Inst.Info.Spec.StaticData {
		if source.Value != nil {
			var values map[string]interface{}
			if err := yaml.Unmarshal(source.Value, &values); err != nil {
				return nil, errors.Wrap(err, "unable to parse value into map")
			}
			data = utils.MergeMaps(data, values)
			continue
		}

		if source.ValueFrom == nil {
			continue
		}

		if source.ValueFrom.SecretKeyRef != nil {
			values, err := GetDataFromSecretKeyRef(ctx, o.Client(), source.ValueFrom.SecretKeyRef, o.Inst.Info.Namespace)
			if err != nil {
				return nil, err
			}
			data = utils.MergeMaps(data, values)
			continue
		}
		if source.ValueFrom.SecretLabelSelector != nil {
			values, err := GetDataFromSecretLabelSelectorRef(ctx, o.Client(), source.ValueFrom.SecretLabelSelector, o.Inst.Info.Namespace)
			if err != nil {
				return nil, err
			}
			data = utils.MergeMaps(data, values)
		}
	}
	o.staticData = data
	return data, nil
}

// TriggerDependants triggers all installations that depend on the current installation.
// These are most likely all installation that import a key which is exported by the current installation.
func (o *Operation) TriggerDependants(ctx context.Context) error {
	for _, sibling := range o.Context().Siblings {
		if !importsAnyExport(o.Inst, sibling) {
			continue
		}

		// todo: maybe use patch
		metav1.SetMetaDataAnnotation(&sibling.Info.ObjectMeta, lsv1alpha1.OperationAnnotation, string(lsv1alpha1.ReconcileOperation))
		if err := o.Client().Update(ctx, sibling.Info); err != nil {
			return errors.Wrapf(err, "unable to trigger installation %s", sibling.Info.Name)
		}
	}
	return nil
}

// GetExportConfigGeneration returns the new export generation of the installation
// based on its own generation and its context
func (o *Operation) SetExportConfigGeneration(ctx context.Context) error {
	// we have to set our config generation to the desired state

	o.Inst.Info.Status.ConfigGeneration = "abc"
	return o.Client().Status().Update(ctx, o.Inst.Info)
}

// UpdateExportReference updates the data object that holds the exported values of the installation.
func (o *Operation) UpdateExportReference(ctx context.Context, values interface{}) error {
	configGen, err := CreateGenerationHash(o.Inst.Info)
	if err != nil {
		return err
	}

	obj := &corev1.Secret{}
	obj.Name = fmt.Sprintf("%s-exports", o.Inst.Info.Name)
	obj.Namespace = o.Inst.Info.Namespace
	if o.Inst.Info.Status.ExportReference != nil {
		obj.Name = o.Inst.Info.Status.ExportReference.Name
		obj.Namespace = o.Inst.Info.Status.ExportReference.Namespace
	}
	data, err := yaml.Marshal(values)
	if err != nil {
		return err
	}

	if _, err := kubernetesutil.CreateOrUpdate(ctx, o.Client(), obj, func() error {
		obj.Data = map[string][]byte{
			lsv1alpha1.DataObjectSecretDataKey: data,
		}
		return controllerutil.SetOwnerReference(o.Inst.Info, obj, kubernetes.LandscaperScheme)
	}); err != nil {
		return err
	}

	o.Inst.Info.Status.ExportReference = &lsv1alpha1.ObjectReference{
		Name:      obj.Name,
		Namespace: obj.Namespace,
	}

	o.Inst.Info.Status.ConfigGeneration = configGen

	return o.UpdateInstallationStatus(ctx, o.Inst.Info, o.Inst.Info.Status.Phase)
}

// UpdateImportReference updates the data object that holds the imported values
// todo: make general import/export dataobject creation and update
func (o *Operation) UpdateImportReference(ctx context.Context, values interface{}) error {
	obj := &corev1.Secret{}
	obj.Name = fmt.Sprintf("%s-imports", o.Inst.Info.Name)
	obj.Namespace = o.Inst.Info.Namespace
	if o.Inst.Info.Status.ImportReference != nil {
		obj.Name = o.Inst.Info.Status.ImportReference.Name
		obj.Namespace = o.Inst.Info.Status.ImportReference.Namespace
	}

	data, err := yaml.Marshal(values)
	if err != nil {
		return err
	}

	if _, err := kubernetesutil.CreateOrUpdate(ctx, o.Client(), obj, func() error {
		obj.Data = map[string][]byte{
			lsv1alpha1.DataObjectSecretDataKey: data,
		}
		return controllerutil.SetOwnerReference(o.Inst.Info, obj, kubernetes.LandscaperScheme)
	}); err != nil {
		return err
	}

	o.Inst.Info.Status.ImportReference = &lsv1alpha1.ObjectReference{
		Name:      obj.Name,
		Namespace: obj.Namespace,
	}
	return o.UpdateInstallationStatus(ctx, o.Inst.Info, o.Inst.Info.Status.Phase)
}

func importsAnyExport(exporter, importer *Installation) bool {
	for _, export := range exporter.Info.Spec.Exports {
		if _, err := importer.GetImportMappingFrom(export.To); err != nil {
			return true
		}
	}
	return false
}
