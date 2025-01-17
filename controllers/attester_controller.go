/*

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

package controllers

import (
	"bytes"
	"context"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/liatrio/rode/api/util"
	rodev1alpha1 "github.com/liatrio/rode/api/v1alpha1"
	"github.com/liatrio/rode/pkg/attester"
)

// AttesterReconciler reconciles a Attester object
type AttesterReconciler struct {
	client.Client
	Log       logr.Logger
	Scheme    *runtime.Scheme
	Attesters map[string]attester.Attester
}

// ListAttesters returns a list of Attester objects
func (r *AttesterReconciler) ListAttesters() map[string]attester.Attester {
	return r.Attesters
}

var (
	attesterFinalizerName = "attester.finalizers.rode.liatr.io"
)

// +kubebuilder:rbac:groups=rode.liatr.io,resources=attesters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rode.liatr.io,resources=attesters/status,verbs=get;update;patch

// Reconcile runs whenever a change to an Attester is made. It attempts to match the current state of the attester to the desired state.
// nolint: gocyclo
func (r *AttesterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("attester", req.NamespacedName)
	opaTrace := false

	log.Info("Reconciling attester")

	att := &rodev1alpha1.Attester{}
	err := r.Get(ctx, req.NamespacedName, att)
	if err != nil {
		log.Error(err, "Unable to load attester")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Register finalizer
	err = r.registerFinalizer(log, att)
	if err != nil {
		log.Error(err, "Error registering finalizer")
	}

	// If the attester is being deleted then remove the finalizer, delete the secret,
	// and remove the Attester object from r.Attesters
	if !att.ObjectMeta.DeletionTimestamp.IsZero() && containsFinalizer(att.ObjectMeta.Finalizers, attesterFinalizerName) {
		log.Info("Removing finalizer")

		// Removing finalizer
		att.ObjectMeta.Finalizers = removeFinalizer(att.ObjectMeta.Finalizers, attesterFinalizerName)
		err := r.Update(ctx, att)
		if err != nil {
			log.Error(err, "Error Removing the finalizer")
			return ctrl.Result{}, err
		}

		// Deleting secret
		err = attester.DeleteSecret(ctx, att, r.Client, types.NamespacedName{
			Name:      att.Spec.PgpSecret,
			Namespace: req.Namespace,
		})
		if err != nil {
			log.Error(err, "Failed to delete the secret")
		}

		// Deleting attester object
		delete(r.Attesters, req.NamespacedName.String())

		return ctrl.Result{}, err
	}

	// If there are 0 conditions then initialize conditions by adding two with false statuses
	if len(att.Status.Conditions) == 0 {
		policyCondition := rodev1alpha1.Condition{
			Type:   rodev1alpha1.ConditionCompiled,
			Status: rodev1alpha1.ConditionStatusFalse,
		}

		att.Status.Conditions = append(att.Status.Conditions, policyCondition)

		secretCondition := rodev1alpha1.Condition{
			Type:   rodev1alpha1.ConditionSecret,
			Status: rodev1alpha1.ConditionStatusFalse,
		}

		att.Status.Conditions = append(att.Status.Conditions, secretCondition)

		if err := r.Status().Update(ctx, att); err != nil {
			log.Error(err, "Unable to initialize attester status")
			return ctrl.Result{}, err
		}
	}

	// Always recompile the policy
	policy, err := attester.NewPolicy(req.Name, att.Spec.Policy, opaTrace)
	if err != nil {
		log.Error(err, "Unable to create policy")

		err = r.updateStatus(ctx, att, rodev1alpha1.ConditionCompiled, rodev1alpha1.ConditionStatusFalse)
		if err != nil {
			log.Error(err, "Unable to update Attester's compiled status to false")
		}

		return ctrl.Result{}, err
	}

	if att.Status.Conditions[0].Status != rodev1alpha1.ConditionStatusTrue {
		err = r.updateStatus(ctx, att, rodev1alpha1.ConditionCompiled, rodev1alpha1.ConditionStatusTrue)
		if err != nil {
			log.Error(err, "Unable to update Attester's compiled status to true")
		}

		log.Info("Setting Policy status to true")
		// Return to avoid race condition on att
		return ctrl.Result{}, nil
	}

	signerSecret := &corev1.Secret{}
	var signer attester.Signer

	// If there isn't already a secret name specified, use req.Name
	if att.Spec.PgpSecret == "" {
		att.Spec.PgpSecret = req.Name
		err = r.Update(ctx, att)
		if err != nil {
			log.Error(err, "Could not update the Attester's PgpSecret field")
			return ctrl.Result{}, err
		}

		log.Info("Setting PgpSecret to req.Name")
		// Return to avoid race condition
		return ctrl.Result{}, nil
	}

	// Check that the secret exists, if it does, recreate a signer from the secret
	err = r.Get(ctx, types.NamespacedName{
		Name:      att.Spec.PgpSecret,
		Namespace: req.Namespace,
	}, signerSecret)
	if err != nil {
		// If the secret wasn't found then create the secret
		if !errors.IsNotFound(err) {
			log.Error(err, "Unable to get the secret")
			return ctrl.Result{}, err
		}

		log.Info("Couldn't find secret, creating a new one")

		signer, err = attester.NewSecret(ctx, att, r.Client, types.NamespacedName{
			Namespace: req.Namespace,
			Name:      att.Spec.PgpSecret,
		})
		if err != nil {
			log.Error(err, "Failed to create the signer secret")

			err = r.updateStatus(ctx, att, rodev1alpha1.ConditionSecret, rodev1alpha1.ConditionStatusFalse)
			if err != nil {
				log.Error(err, "Unable to update Attester's secret status to false")
			}
			return ctrl.Result{}, err
		}

		// Update the status to true
		err = r.updateStatus(ctx, att, rodev1alpha1.ConditionSecret, rodev1alpha1.ConditionStatusTrue)
		if err != nil {
			log.Error(err, "Unable to update Attester's secret status to true")
		}

		log.Info("Created the signer secret")
	} else {
		// The secret does exist, recreate the signer from the secret
		buf := bytes.NewBuffer(signerSecret.Data["keys"])

		signer, err = attester.ReadSigner(buf)
		if err != nil {
			log.Error(err, "Unable to create signer from secret")
			err = r.updateStatus(ctx, att, rodev1alpha1.ConditionSecret, rodev1alpha1.ConditionStatusFalse)
			return ctrl.Result{}, err
		}

		err = r.updateStatus(ctx, att, rodev1alpha1.ConditionSecret, rodev1alpha1.ConditionStatusTrue)
		if err != nil {
			log.Error(err, "Unable to update Attester's secret status to true")
		}
	}

	// Create the attester if it doesn't already exist, otherwise update it
	r.Attesters[req.NamespacedName.String()] = attester.NewAttester(req.NamespacedName.String(), policy, signer)

	return ctrl.Result{}, nil
}

func (r *AttesterReconciler) registerFinalizer(logger logr.Logger, attester *rodev1alpha1.Attester) error {
	// If the attester isn't being deleted and it doesn't contain a finalizer, then add one
	if attester.ObjectMeta.DeletionTimestamp.IsZero() && !containsFinalizer(attester.ObjectMeta.Finalizers, attesterFinalizerName) {
		logger.Info("Creating attester finalizer...")
		attester.ObjectMeta.Finalizers = append(attester.ObjectMeta.Finalizers, attesterFinalizerName)

		if err := r.Update(context.Background(), attester); err != nil {
			return err
		}
	}

	return nil
}

func (r *AttesterReconciler) updateStatus(ctx context.Context, attester *rodev1alpha1.Attester, conditionType rodev1alpha1.ConditionType, status rodev1alpha1.ConditionStatus) error {
	if conditionType == rodev1alpha1.ConditionCompiled {
		attester.Status.Conditions[0].Status = status
	}

	if conditionType == rodev1alpha1.ConditionSecret {
		attester.Status.Conditions[1].Status = status
	}

	if err := r.Status().Update(ctx, attester); err != nil {
		return err
	}

	return nil
}

// SetupWithManager sets up the watching of Attester objects and filters out the events we don't want to watch
func (r *AttesterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rodev1alpha1.Attester{}).
		WithEventFilter(ignoreConditionStatusUpdateToActive(attesterToConditioner, rodev1alpha1.ConditionCompiled)).
		WithEventFilter(ignoreConditionStatusUpdateToActive(attesterToConditioner, rodev1alpha1.ConditionSecret)).
		WithEventFilter(ignoreFinalizerUpdate()).
		WithEventFilter(ignoreDelete()).
		Complete(r)
}

// AttesterToConditioner takes an Attester and returns a util.Conditioner
func attesterToConditioner(o runtime.Object) util.Conditioner {
	return o.(*rodev1alpha1.Attester)
}
