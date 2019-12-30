/*
Copyright 2019 The Kubernetes Authors.

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
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1alpha3"
	kubeadmv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/types/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/controllers/noderefutil"
	"sigs.k8s.io/cluster-api/controllers/remote"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1alpha3"
	capierrors "sigs.k8s.io/cluster-api/errors"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/secret"
)

// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kubeadmcontrolplanes;kubeadmcontrolplanes/status,verbs=get;list;watch;create;update;patch;delete

// KubeadmControlPlaneReconciler reconciles a KubeadmControlPlane object
type KubeadmControlPlaneReconciler struct {
	Client client.Client
	Log    logr.Logger
	scheme *runtime.Scheme

	// for testing
	remoteClient func(client.Client, *clusterv1.Cluster, *runtime.Scheme) (client.Client, error)

	controller controller.Controller
	recorder   record.EventRecorder
}

func (r *KubeadmControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager, options controller.Options) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&controlplanev1.KubeadmControlPlane{}).
		Owns(&clusterv1.Machine{}).
		Watches(
			&source.Kind{Type: &clusterv1.Cluster{}},
			&handler.EnqueueRequestsFromMapFunc{ToRequests: handler.ToRequestsFunc(r.ClusterToKubeadmControlPlane)},
		).
		WithOptions(options).
		Build(r)

	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}

	r.scheme = mgr.GetScheme()
	r.controller = c
	r.recorder = mgr.GetEventRecorderFor("kubeadm-control-plane-controller")

	if r.remoteClient == nil {
		r.remoteClient = remote.NewClusterClient
	}

	return nil
}

func (r *KubeadmControlPlaneReconciler) Reconcile(req ctrl.Request) (res ctrl.Result, _ error) {
	logger := r.Log.WithValues("kubeadmControlPlane", req.Name, "namespace", req.Namespace)
	ctx := context.Background()

	// Fetch the KubeadmControlPlane instance.
	kubeadmControlPlane := &controlplanev1.KubeadmControlPlane{}
	if err := r.Client.Get(ctx, req.NamespacedName, kubeadmControlPlane); err != nil {
		if apierrors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return ctrl.Result{}, nil
		}

		// Error reading the object - requeue the request.
		logger.Error(err, "Failed to retrieve requested KubeadmControlPlane resource from the API Server")
		return ctrl.Result{Requeue: true}, nil
	}

	// Initialize the patch helper.
	patchHelper, err := patch.NewHelper(kubeadmControlPlane, r.Client)
	if err != nil {
		logger.Error(err, "Failed to configure the patch helper")
		return ctrl.Result{Requeue: true}, nil
	}

	defer func() {
		// Always attempt to Patch the KubeadmControlPlane object and status after each reconciliation.
		if patchErr := patchHelper.Patch(ctx, kubeadmControlPlane); patchErr != nil {
			logger.Error(patchErr, "Failed to patch KubeadmControlPlane")
			res.Requeue = true
		}
	}()

	// Handle deletion reconciliation loop.
	if !kubeadmControlPlane.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, kubeadmControlPlane, logger)
	}

	// Handle normal reconciliation loop.
	return r.reconcile(ctx, kubeadmControlPlane, logger)
}

// reconcile handles KubeadmControlPlane reconciliation.
func (r *KubeadmControlPlaneReconciler) reconcile(ctx context.Context, kcp *controlplanev1.KubeadmControlPlane, logger logr.Logger) (_ ctrl.Result, retErr error) {
	// If object doesn't have a finalizer, add one.
	controllerutil.AddFinalizer(kcp, controlplanev1.KubeadmControlPlaneFinalizer)

	// Fetch the Cluster.
	cluster, err := util.GetOwnerCluster(ctx, r.Client, kcp.ObjectMeta)
	if err != nil {
		logger.Error(err, "Failed to retrieve owner Cluster from the API Server")
		return ctrl.Result{}, err
	}
	if cluster == nil {
		logger.Info("Cluster Controller has not yet set OwnerRef")
		return ctrl.Result{}, nil
	}
	logger = logger.WithValues("cluster", cluster.Name)

	// TODO: handle proper adoption of Machines
	ownedMachines, err := r.getOwnedMachines(
		ctx,
		kcp,
		types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name},
	)
	if err != nil {
		logger.Error(err, "failed to get list of owned machines")
		return ctrl.Result{}, err
	}

	// Always attempt to update status
	defer func() {
		if updateErr := r.updateStatus(ctx, kcp, cluster); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
			if retErr == nil {
				retErr = updateErr
			} else {
				retErr = utilerrors.NewAggregate([]error{retErr, updateErr})
			}
		}
	}()

	// Generate Cluster Certificates if needed
	config := kcp.Spec.KubeadmConfigSpec.DeepCopy()
	config.JoinConfiguration = nil
	if config.ClusterConfiguration == nil {
		config.ClusterConfiguration = &kubeadmv1.ClusterConfiguration{}
	}
	certificates := secret.NewCertificatesForInitialControlPlane(config.ClusterConfiguration)
	err = certificates.LookupOrGenerate(
		ctx,
		r.Client,
		types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name},
		*metav1.NewControllerRef(kcp, controlplanev1.GroupVersion.WithKind("KubeadmControlPlane")),
	)
	if err != nil {
		logger.Error(err, "unable to lookup or create cluster certificates")
		return ctrl.Result{}, err
	}

	// If ControlPlaneEndpoint is not set, return early
	if cluster.Spec.ControlPlaneEndpoint.IsZero() {
		logger.Info("Cluster does not yet have a ControlPlaneEndpoint defined")
		return ctrl.Result{}, nil
	}

	// Generate Cluster Kubeconfig if needed
	err = r.reconcileKubeconfig(
		ctx,
		types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name},
		cluster.Spec.ControlPlaneEndpoint,
		kcp,
	)
	if err != nil {
		if requeueErr, ok := errors.Cause(err).(capierrors.HasRequeueAfterError); ok {
			logger.Error(err, "required certificates not found, requeueing")
			return ctrl.Result{
				Requeue:      true,
				RequeueAfter: requeueErr.GetRequeueAfter(),
			}, nil
		}
		logger.Error(err, "failed to reconcile Kubeconfig")
		return ctrl.Result{}, err
	}

	// Currently we are not handling upgrade, so treat all owned machines as one for now.
	// Once we start handling upgrade, we'll need to filter this list and act appropriately
	numMachines := len(ownedMachines)
	desiredReplicas := int(*kcp.Spec.Replicas)
	switch {
	// We are creating the first replica
	case numMachines < desiredReplicas && numMachines == 0:
		// create new Machine w/ init
		logger.Info("Scaling to 1", "Desired Replicas", desiredReplicas, "Existing Replicas", numMachines)
		if err := r.initializeControlPlane(ctx, cluster, kcp, logger); err != nil {
			logger.Error(err, "Failed to initialize the Control Plane")
			return ctrl.Result{}, err
		}
	// scaling up
	case numMachines < desiredReplicas && numMachines > 0:
		logger.Info("Scaling up", "Desired Replicas", desiredReplicas, "Existing Replicas", numMachines)
		// create a new Machine w/ join
		err := errors.New("Not Implemented")
		logger.Error(err, "Should create new Machine using join here.")
		return ctrl.Result{}, err
	// scaling down
	case numMachines > desiredReplicas:
		logger.Info("Scaling down", "Desired Replicas", desiredReplicas, "Existing Replicas", numMachines)
		err := errors.New("Not Implemented")
		logger.Error(err, "Should delete the appropriate Machine here.")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *KubeadmControlPlaneReconciler) updateStatus(ctx context.Context, kcp *controlplanev1.KubeadmControlPlane, cluster *clusterv1.Cluster) error {
	labelSelector := generateKubeadmControlPlaneSelector(cluster.Name)
	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		// Since we are building up the LabelSelector above, this should not fail
		return errors.Wrap(err, "failed to parse label selector")
	}
	// Copy label selector to its status counterpart in string format.
	// This is necessary for CRDs including scale subresources.
	kcp.Status.Selector = selector.String()

	ownedMachines, err := r.getOwnedMachines(
		ctx,
		kcp,
		types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name},
	)
	if err != nil {
		return errors.Wrap(err, "failed to get list of owned machines")
	}

	replicas := int32(len(ownedMachines))
	// TODO: take into account configuration hash once upgrades are in place
	kcp.Status.Replicas = replicas

	remoteClient, err := r.remoteClient(r.Client, cluster, r.scheme)
	if err != nil && !apierrors.IsNotFound(errors.Cause(err)) {
		return errors.Wrap(err, "failed to create remote cluster client")
	}

	readyMachines := int32(0)
	for _, m := range ownedMachines {

		node, err := getMachineNode(ctx, remoteClient, m)
		if err != nil {
			return errors.Wrap(err, "failed to get referenced Node")
		}
		if node == nil {
			continue
		}
		if noderefutil.IsNodeReady(node) {
			readyMachines++
		}
	}
	kcp.Status.ReadyReplicas = readyMachines
	kcp.Status.UnavailableReplicas = replicas - readyMachines

	if !kcp.Status.Initialized {
		if kcp.Status.ReadyReplicas > 0 {
			kcp.Status.Initialized = true
		}
	}
	return nil
}

func (r *KubeadmControlPlaneReconciler) initializeControlPlane(ctx context.Context, cluster *clusterv1.Cluster, kcp *controlplanev1.KubeadmControlPlane, logger logr.Logger) error {
	ownerRef := metav1.NewControllerRef(kcp, controlplanev1.GroupVersion.WithKind("KubeadmControlPlane"))
	// Clone the infrastructure template
	infraRef, err := external.CloneTemplate(
		ctx,
		r.Client,
		&kcp.Spec.InfrastructureTemplate,
		kcp.Namespace,
		cluster.Name,
		ownerRef,
	)
	if err != nil {
		return errors.Wrap(err, "Failed to clone infrastructure template")
	}

	// Create the bootstrap configuration
	bootstrapSpec := kcp.Spec.KubeadmConfigSpec.DeepCopy()
	bootstrapSpec.JoinConfiguration = nil
	bootstrapRef, err := r.generateKubeadmConfig(
		ctx,
		kcp.Namespace,
		kcp.Name,
		cluster.Name,
		bootstrapSpec,
		ownerRef,
	)
	if err != nil {
		return errors.Wrap(err, "Failed to generate bootstrap config")
	}

	// Create the Machine
	err = r.generateMachine(
		ctx,
		kcp.Namespace,
		kcp.Name,
		cluster.Name,
		kcp.Spec.Version,
		infraRef,
		bootstrapRef,
		generateKubeadmControlPlaneLabels(cluster.Name),
		ownerRef,
	)
	if err != nil {
		logger.Error(err, "Unable to create Machine")
		r.recorder.Eventf(kcp, corev1.EventTypeWarning, "FailedCreateMachine", "Failed to create machine: %v", err)

		errs := []error{err}

		infraConfig := &unstructured.Unstructured{}
		infraConfig.SetKind(infraRef.Kind)
		infraConfig.SetAPIVersion(infraRef.APIVersion)
		infraConfig.SetNamespace(infraRef.Namespace)
		infraConfig.SetName(infraRef.Name)

		if err := r.Client.Delete(context.TODO(), infraConfig); !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to cleanup infrastructure configuration object after Machine creation error")
			errs = append(errs, err)
		}

		bootstrapConfig := &bootstrapv1.KubeadmConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:      bootstrapRef.Name,
				Namespace: bootstrapRef.Namespace,
			},
		}

		if err := r.Client.Delete(context.TODO(), bootstrapConfig); !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to cleanup bootstrap configuration object after Machine creation error")
			errs = append(errs, err)
		}

		return utilerrors.NewAggregate(errs)
	}

	return nil
}

func (r *KubeadmControlPlaneReconciler) generateKubeadmConfig(ctx context.Context, namespace, namePrefix, clusterName string, spec *bootstrapv1.KubeadmConfigSpec, owner *metav1.OwnerReference) (*corev1.ObjectReference, error) {
	bootstrapConfig := &bootstrapv1.KubeadmConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.SimpleNameGenerator.GenerateName(namePrefix + "-"),
			Namespace: namespace,
			Labels:    map[string]string{clusterv1.ClusterLabelName: clusterName},
		},
		Spec: *spec,
	}
	if owner != nil {
		bootstrapConfig.SetOwnerReferences([]metav1.OwnerReference{*owner})
	}
	if err := r.Client.Create(ctx, bootstrapConfig); err != nil {
		return nil, errors.Wrap(err, "Failed to create bootstrap configuration")
	}

	bootstrapRef := &corev1.ObjectReference{
		APIVersion: bootstrapv1.GroupVersion.String(),
		Kind:       "KubeadmConfig",
		Name:       bootstrapConfig.GetName(),
		Namespace:  bootstrapConfig.GetNamespace(),
		UID:        bootstrapConfig.GetUID(),
	}

	return bootstrapRef, nil
}

func (r *KubeadmControlPlaneReconciler) generateMachine(ctx context.Context, namespace, namePrefix, clusterName, version string, infraRef, bootstrapRef *corev1.ObjectReference, labels map[string]string, owner *metav1.OwnerReference) error {
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Labels:    labels,
			Namespace: namespace,
			Name:      names.SimpleNameGenerator.GenerateName(namePrefix + "-"),
		},
		Spec: clusterv1.MachineSpec{
			ClusterName:       clusterName,
			Version:           &version,
			InfrastructureRef: *infraRef,
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: bootstrapRef,
			},
		},
	}

	if owner != nil {
		machine.SetOwnerReferences([]metav1.OwnerReference{*owner})
	}

	if err := r.Client.Create(ctx, machine); err != nil {
		return errors.Wrap(err, "Failed to create machine")
	}

	return nil
}

func generateKubeadmControlPlaneSelector(clusterName string) *metav1.LabelSelector {
	return &metav1.LabelSelector{
		MatchLabels: generateKubeadmControlPlaneLabels(clusterName),
	}
}

func generateKubeadmControlPlaneLabels(clusterName string) map[string]string {
	return map[string]string{
		clusterv1.ClusterLabelName:             clusterName,
		clusterv1.MachineControlPlaneLabelName: "",
	}
}

// reconcileDelete handles KubeadmControlPlane deletion.
func (r *KubeadmControlPlaneReconciler) reconcileDelete(_ context.Context, kcp *controlplanev1.KubeadmControlPlane, logger logr.Logger) (ctrl.Result, error) {
	err := errors.New("Not Implemented")

	if err != nil {
		logger.Error(err, "Not Implemented")
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(kcp, controlplanev1.KubeadmControlPlaneFinalizer)
	return ctrl.Result{}, nil
}

func (r *KubeadmControlPlaneReconciler) reconcileKubeconfig(ctx context.Context, clusterName types.NamespacedName, endpoint clusterv1.APIEndpoint, kcp *controlplanev1.KubeadmControlPlane) error {
	if endpoint.IsZero() {
		return nil
	}

	_, err := secret.GetFromNamespacedName(r.Client, clusterName, secret.Kubeconfig)
	switch {
	case apierrors.IsNotFound(err):
		createErr := kubeconfig.CreateSecretWithOwner(
			ctx,
			r.Client,
			clusterName,
			endpoint.String(),
			*metav1.NewControllerRef(kcp, controlplanev1.GroupVersion.WithKind("KubeadmControlPlane")),
		)
		if createErr != nil {
			if createErr == kubeconfig.ErrDependentCertificateNotFound {
				return errors.Wrapf(&capierrors.RequeueAfterError{RequeueAfter: 30 * time.Second},
					"could not find secret %q for Cluster %q in namespace %q, requeuing",
					secret.ClusterCA, clusterName.Name, clusterName.Namespace)
			}
			return createErr
		}
	case err != nil:
		return errors.Wrapf(err, "failed to retrieve Kubeconfig Secret for Cluster %q in namespace %q", clusterName.Name, clusterName.Namespace)
	}

	return nil
}

func (r *KubeadmControlPlaneReconciler) getMachines(ctx context.Context, clusterName types.NamespacedName) (*clusterv1.MachineList, error) {
	selector := generateKubeadmControlPlaneLabels(clusterName.Name)
	allMachines := &clusterv1.MachineList{}
	if err := r.Client.List(ctx, allMachines, client.InNamespace(clusterName.Namespace), client.MatchingLabels(selector)); err != nil {
		return nil, errors.Wrap(err, "failed to list machines")
	}
	return allMachines, nil
}

func (r *KubeadmControlPlaneReconciler) getOwnedMachines(ctx context.Context, kcp *controlplanev1.KubeadmControlPlane, clusterName types.NamespacedName) ([]*clusterv1.Machine, error) {
	allMachines, err := r.getMachines(ctx, clusterName)
	if err != nil {
		return nil, err
	}

	var ownedMachines []*clusterv1.Machine
	for i := range allMachines.Items {
		m := allMachines.Items[i]
		controllerRef := metav1.GetControllerOf(&m)
		if controllerRef != nil && controllerRef.Kind == "KubeadmControlPlane" && controllerRef.Name == kcp.Name {
			ownedMachines = append(ownedMachines, &m)
		}
	}

	return ownedMachines, nil
}

func getMachineNode(ctx context.Context, crClient client.Client, machine *clusterv1.Machine) (*corev1.Node, error) {
	nodeRef := machine.Status.NodeRef
	if nodeRef == nil {
		return nil, nil
	}

	node := &corev1.Node{}
	err := crClient.Get(
		ctx,
		types.NamespacedName{Name: nodeRef.Name},
		node,
	)
	if err != nil {
		if apierrors.IsNotFound(errors.Cause(err)) {
			return nil, nil
		}
		return nil, err
	}

	return node, nil
}

// ClusterToKubeadmControlPlane is a handler.ToRequestsFunc to be used to enqeue requests for reconciliation
// for KubeadmControlPlane based on updates to a Cluster.
func (r *KubeadmControlPlaneReconciler) ClusterToKubeadmControlPlane(o handler.MapObject) []ctrl.Request {
	c, ok := o.Object.(*clusterv1.Cluster)
	if !ok {
		r.Log.Error(nil, fmt.Sprintf("Expected a Cluster but got a %T", o.Object))
		return nil
	}

	controlPlaneRef := c.Spec.ControlPlaneRef
	if controlPlaneRef != nil && controlPlaneRef.Kind == "KubeadmControlPlane" {
		name := client.ObjectKey{Namespace: controlPlaneRef.Namespace, Name: controlPlaneRef.Name}
		return []ctrl.Request{{NamespacedName: name}}
	}

	return nil
}
