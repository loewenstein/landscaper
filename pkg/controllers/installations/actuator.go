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
	"io/ioutil"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"

	lsv1alpha1helper "github.com/gardener/landscaper/pkg/apis/core/v1alpha1/helper"
	"github.com/gardener/landscaper/pkg/landscaper/datatype"
	"github.com/gardener/landscaper/pkg/landscaper/installations"
	"github.com/gardener/landscaper/pkg/landscaper/installations/exports"
	"github.com/gardener/landscaper/pkg/landscaper/installations/imports"
	"github.com/gardener/landscaper/pkg/landscaper/installations/subinstallations"
	"github.com/gardener/landscaper/pkg/landscaper/landscapeconfig"
	"github.com/gardener/landscaper/pkg/landscaper/registry"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/runtime/inject"
	"sigs.k8s.io/yaml"

	lsv1alpha1 "github.com/gardener/landscaper/pkg/apis/core/v1alpha1"
)

func NewActuator() (reconcile.Reconciler, error) {
	return &actuator{}, nil
}

type actuator struct {
	log      logr.Logger
	c        client.Client
	scheme   *runtime.Scheme
	registry registry.Registry
}

var _ inject.Client = &actuator{}

var _ inject.Logger = &actuator{}

var _ inject.Scheme = &actuator{}

// InjectClients injects the current kubernetes client into the actuator
func (a *actuator) InjectClient(c client.Client) error {
	a.c = c
	return nil
}

// InjectLogger injects a logging instance into the actuator
func (a *actuator) InjectLogger(log logr.Logger) error {
	a.log = log
	return nil
}

// InjectScheme injects the current scheme into the actuator
func (a *actuator) InjectScheme(scheme *runtime.Scheme) error {
	a.scheme = scheme
	return nil
}

// InjectRegistry injects a Registry into the actuator
func (a *actuator) InjectRegistry(r registry.Registry) error {
	a.registry = r
	return nil
}

func (a *actuator) Reconcile(req reconcile.Request) (reconcile.Result, error) {
	ctx := context.Background()
	defer ctx.Done()
	a.log.Info("reconcile", "resource", req.NamespacedName)

	inst := &lsv1alpha1.ComponentInstallation{}
	if err := a.c.Get(ctx, req.NamespacedName, inst); err != nil {
		a.log.Error(err, "unable to get installation")
		return reconcile.Result{}, err
	}

	// if the inst has the reconcile annotation or if the inst is waiting for dependencies
	// we need to check if all required imports are satisfied.
	if lsv1alpha1helper.HasOperation(inst.ObjectMeta, lsv1alpha1.ReconcileOperation) {
		delete(inst.Annotations, lsv1alpha1.OperationAnnotation)
		if err := a.c.Update(ctx, inst); err != nil {
			return reconcile.Result{Requeue: true}, err
		}
		return reconcile.Result{}, nil
	}

	definition, err := a.registry.GetDefinitionByRef(inst.Spec.DefinitionRef)
	if err != nil {
		a.log.Error(err, "unable to get definition")
		return reconcile.Result{}, err
	}

	internalInstallation, err := installations.New(inst, definition)
	if err != nil {
		a.log.Error(err, "unable to create internal representation of installation")
		return reconcile.Result{}, err
	}

	datatypeList := &lsv1alpha1.DataTypeList{}
	if err := a.c.List(ctx, datatypeList); err != nil {
		a.log.Error(err, "unable to list all datatypes")
		return reconcile.Result{}, err
	}
	datatypes, err := datatype.CreateDatatypesMap(datatypeList.Items)
	if err != nil {
		a.log.Error(err, "unable to parse datatypes")
		return reconcile.Result{}, err
	}

	op := installations.NewOperation(a.log, a.c, a.scheme, a.registry, datatypes)

	// for debugging read landscape from tmp file
	landscapeConfig := make(map[string]interface{})
	data, err := ioutil.ReadFile("./tmp/ls-config.yaml")
	if err != nil {
		return reconcile.Result{}, err
	}
	if err := yaml.Unmarshal(data, &landscapeConfig); err != nil {
		return reconcile.Result{}, err
	}

	// todo: get lsconfig
	if err := a.Ensure(ctx, op, nil, internalInstallation); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (a *actuator) Ensure(ctx context.Context, op installations.Operation, landscapeConfig *landscapeconfig.LandscapeConfig, inst *installations.Installation) error {
	inst.Info.Status.Phase = lsv1alpha1.ComponentPhasePending
	if err := a.c.Status().Update(ctx, inst.Info); err != nil {
		return err
	}

	// check that all referenced definitions have a corresponding installation
	subinstallation := subinstallations.New(op)
	if err := subinstallation.Ensure(ctx, inst.Info, inst.Definition); err != nil {
		a.log.Error(err, "unable to ensure sub installations")
		return err
	}

	// generate the current context for the installation.
	instOp, err := installations.NewInstallationOperation(ctx, op, inst)
	if err != nil {
		return errors.Wrapf(err, "unable to create installation context")
	}

	validator := imports.NewValidator(op, landscapeConfig, instOp.Context().Parent, instOp.Context().Siblings...)
	if err := validator.Validate(inst); err != nil {
		a.log.Error(err, "unable to validate imports")
		return err
	}

	// as all imports are satisfied we can collect and merge all imports
	// and then start the executions

	inst.Info.Status.Phase = lsv1alpha1.ComponentPhaseProgressing
	if err := a.c.Status().Update(ctx, inst.Info); err != nil {
		return err
	}

	// only needed if execution are processed
	constructor := imports.NewConstructor(op, landscapeConfig, instOp.Context().Parent, instOp.Context().Siblings...)
	_, err = constructor.Construct(ctx, inst)
	if err != nil {
		a.log.Error(err, "unable to collect imports")
		return err
	}

	if err := subinstallation.TriggerSubInstallations(ctx, inst.Info); err != nil {
		return err
	}

	if err := a.runExecutions(ctx, inst.Info, nil); err != nil {
		a.log.Error(err, "error during execution")
		return err
	}

	// when all executions are finished and the exports are uploaded
	// we have to validate the uploaded exports
	if err := exports.NewValidator(op).Validate(ctx, inst); err != nil {
		a.log.Error(err, "error during export validation")
		return err
	}

	// update import status
	inst.Info.Status.Phase = lsv1alpha1.ComponentPhaseCompleted
	inst.Info.Status.Imports = inst.ImportStatus().GetStates()
	if err := a.c.Status().Update(ctx, inst.Info); err != nil {
		return err
	}

	// as all exports are validated, lets trigger dependant components
	// todo: check if this is a must, maybe track what we already successfully triggered
	if err := instOp.TriggerDependants(ctx); err != nil {
		a.log.Error(err, "error during dependant trigger")
		return err
	}
	return nil
}
