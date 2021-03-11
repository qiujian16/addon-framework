package managedcluster

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/open-cluster-management/addon-framework/pkg/hub/controllers/managedcluster/bindata"
	clientset "github.com/open-cluster-management/api/client/cluster/clientset/versioned"
	informerv1 "github.com/open-cluster-management/api/client/cluster/informers/externalversions/cluster/v1"
	listerv1 "github.com/open-cluster-management/api/client/cluster/listers/cluster/v1"
	v1 "github.com/open-cluster-management/api/cluster/v1"

	"github.com/openshift/library-go/pkg/assets"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	operatorhelpers "github.com/openshift/library-go/pkg/operator/v1helpers"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	manifestDir             = "pkg/hub/managedcluster"
	managedClusterFinalizer = "cluster.open-cluster-management.io/addon-resource-cleanup"
)

var staticFiles = []string{
	"manifests/managedcluster-clusterrole.yaml",
	"manifests/managedcluster-clusterrolebinding.yaml",
}

// managedClusterController reconciles instances of ManagedCluster on the hub.
type managedClusterController struct {
	kubeClient    kubernetes.Interface
	clusterClient clientset.Interface
	clusterLister listerv1.ManagedClusterLister
	eventRecorder events.Recorder
}

// NewManagedClusterController creates a new managed cluster controller
func NewManagedClusterController(
	kubeClient kubernetes.Interface,
	clusterClient clientset.Interface,
	clusterInformer informerv1.ManagedClusterInformer,
	recorder events.Recorder) factory.Controller {
	c := &managedClusterController{
		kubeClient:    kubeClient,
		clusterClient: clusterClient,
		clusterLister: clusterInformer.Lister(),
		eventRecorder: recorder.WithComponentSuffix("managed-cluster-controller"),
	}
	return factory.New().
		WithInformersQueueKeyFunc(func(obj runtime.Object) string {
			accessor, _ := meta.Accessor(obj)
			return accessor.GetName()
		}, clusterInformer.Informer()).
		WithSync(c.sync).
		ToController("ManagedClusterController", recorder)
}

func (c *managedClusterController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	managedClusterName := syncCtx.QueueKey()
	klog.V(4).Infof("Reconciling ManagedCluster %s", managedClusterName)
	managedCluster, err := c.clusterLister.Get(managedClusterName)
	if errors.IsNotFound(err) {
		// Spoke cluster not found, could have been deleted, do nothing.
		return nil
	}
	if err != nil {
		return err
	}

	managedCluster = managedCluster.DeepCopy()
	if managedCluster.DeletionTimestamp.IsZero() {
		hasFinalizer := false
		for i := range managedCluster.Finalizers {
			if managedCluster.Finalizers[i] == managedClusterFinalizer {
				hasFinalizer = true
				break
			}
		}
		if !hasFinalizer {
			managedCluster.Finalizers = append(managedCluster.Finalizers, managedClusterFinalizer)
			_, err := c.clusterClient.ClusterV1().ManagedClusters().Update(ctx, managedCluster, metav1.UpdateOptions{})
			return err
		}
	}

	// Spoke cluster is deleting, we remove its related resources
	if !managedCluster.DeletionTimestamp.IsZero() {
		if err := c.removeManagedClusterResources(ctx, managedClusterName); err != nil {
			return err
		}
		return c.removeManagedClusterFinalizer(ctx, managedCluster)
	}

	// TODO: we will add the managedcluster-namespace.yaml back to staticFiles
	// in next release, currently, we need keep the namespace after the managed
	// cluster is deleted, see the issue
	// https://github.com/open-cluster-management/backlog/issues/2648
	applyFiles := []string{"manifests/managedcluster-namespace.yaml"}
	applyFiles = append(applyFiles, staticFiles...)

	// Hub cluster-admin accepts the spoke cluster, we apply
	// 1. clusterrole and clusterrolebinding for this spoke cluster.
	// 2. namespace for this spoke cluster.
	// 3. role and rolebinding for this spoke cluster on its namespace.
	resourceResults := resourceapply.ApplyDirectly(
		resourceapply.NewKubeClientHolder(c.kubeClient),
		syncCtx.Recorder(),
		managedClusterAssetFn(manifestDir, managedClusterName),
		applyFiles...,
	)
	errs := []error{}
	for _, result := range resourceResults {
		if result.Error != nil {
			errs = append(errs, fmt.Errorf("%q (%T): %v", result.File, result.Type, result.Error))
		}
	}

	return operatorhelpers.NewMultiLineAggregate(errs)
}

func (c *managedClusterController) removeManagedClusterResources(ctx context.Context, managedClusterName string) error {
	errs := []error{}
	name := fmt.Sprintf("open-cluster-management:managedcluster:addon:%s", managedClusterName)
	err := c.kubeClient.RbacV1().ClusterRoles().Delete(ctx, name, metav1.DeleteOptions{})
	if !errors.IsNotFound(err) {
		return err
	}
	err = c.kubeClient.RbacV1().ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{})
	if !errors.IsNotFound(err) {
		return err
	}
	return operatorhelpers.NewMultiLineAggregate(errs)
}

func (c *managedClusterController) removeManagedClusterFinalizer(ctx context.Context, managedCluster *v1.ManagedCluster) error {
	copiedFinalizers := []string{}
	for i := range managedCluster.Finalizers {
		if managedCluster.Finalizers[i] == managedClusterFinalizer {
			continue
		}
		copiedFinalizers = append(copiedFinalizers, managedCluster.Finalizers[i])
	}

	if len(managedCluster.Finalizers) != len(copiedFinalizers) {
		managedCluster.Finalizers = copiedFinalizers
		_, err := c.clusterClient.ClusterV1().ManagedClusters().Update(ctx, managedCluster, metav1.UpdateOptions{})
		return err
	}

	return nil
}

func managedClusterAssetFn(manifestDir, managedClusterName string) resourceapply.AssetFunc {
	return func(name string) ([]byte, error) {
		config := struct {
			ManagedClusterName string
		}{
			ManagedClusterName: managedClusterName,
		}
		return assets.MustCreateAssetFromTemplate(name, bindata.MustAsset(filepath.Join(manifestDir, name)), config).Data, nil
	}
}
