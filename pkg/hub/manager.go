package hub

import (
	"context"
	"time"

	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"k8s.io/client-go/kubernetes"

	"github.com/open-cluster-management/addon-framework/pkg/hub/controllers/lease"
	"github.com/open-cluster-management/addon-framework/pkg/hub/controllers/managedcluster"
	addonclient "github.com/open-cluster-management/api/client/addon/clientset/versioned"
	addoninformers "github.com/open-cluster-management/api/client/addon/informers/externalversions"
	clusterv1client "github.com/open-cluster-management/api/client/cluster/clientset/versioned"
	clusterv1informers "github.com/open-cluster-management/api/client/cluster/informers/externalversions"

	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/rest"
)

// RunControllerManager starts the controllers on hub to manage spoke cluster registration.
func RunControllerManager(ctx context.Context, controllerContext *controllercmd.ControllerContext) error {
	// If qps in kubconfig is not set, increase the qps and burst to enhance the ability of kube client to handle
	// requests in concurrent
	// TODO: Use ClientConnectionOverrides flags to change qps/burst when library-go exposes them in the future
	kubeConfig := rest.CopyConfig(controllerContext.KubeConfig)
	if kubeConfig.QPS == 0.0 {
		kubeConfig.QPS = 100.0
		kubeConfig.Burst = 200
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}

	clusterClient, err := clusterv1client.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}

	addonClient, err := addonclient.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}

	addonInformerFactory := addoninformers.NewSharedInformerFactory(addonClient, 10*time.Minute)
	clusterInformers := clusterv1informers.NewSharedInformerFactory(clusterClient, 10*time.Minute)
	kubeInfomers := kubeinformers.NewSharedInformerFactory(kubeClient, 10*time.Minute)

	leaseController := lease.NewAddonLeaseController(
		kubeClient,
		addonClient,
		addonInformerFactory.Addon().V1alpha1().ManagedClusterAddOns(),
		kubeInfomers.Coordination().V1().Leases(),
		clusterInformers.Cluster().V1().ManagedClusters(),
		5*time.Minute, //TODO: this interval time should be allowed to change from outside
		controllerContext.EventRecorder,
	)

	managedClusterController := managedcluster.NewManagedClusterController(
		kubeClient,
		clusterClient,
		clusterInformers.Cluster().V1().ManagedClusters(),
		controllerContext.EventRecorder,
	)

	go clusterInformers.Start(ctx.Done())
	go addonInformerFactory.Start(ctx.Done())
	go kubeInfomers.Start(ctx.Done())

	go leaseController.Run(ctx, 1)
	go managedClusterController.Run(ctx, 1)

	<-ctx.Done()
	return nil
}
