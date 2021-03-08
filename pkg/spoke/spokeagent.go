package spoke

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/open-cluster-management/addon-framework/pkg/spoke/controllers/clientcertmanager"
	"github.com/open-cluster-management/addon-framework/pkg/spoke/controllers/lease"
	addonclient "github.com/open-cluster-management/api/client/addon/clientset/versioned"
	addoninformers "github.com/open-cluster-management/api/client/addon/informers/externalversions"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	// spokeAgentNameLength is the length of the spoke agent name which is generated automatically
	spokeAgentNameLength = 5
	// defaultSpokeComponentNamespace is the default namespace in which the spoke agent is deployed
	defaultSpokeComponentNamespace = "open-cluster-management"
)

// SpokeAgentOptions holds configuration for spoke cluster agent
type SpokeAgentOptions struct {
	ComponentNamespace string
	ClusterName        string
	HubKubeconfig      string
}

// NewSpokeAgentOptions returns a SpokeAgentOptions
func NewSpokeAgentOptions() *SpokeAgentOptions {
	return &SpokeAgentOptions{
		HubKubeconfig: "/spoke/hub-kubeconfig",
	}
}

func (o *SpokeAgentOptions) RunSpokeAgent(ctx context.Context, controllerContext *controllercmd.ControllerContext) error {
	if err := o.Validate(); err != nil {
		klog.Fatal(err)
	}

	klog.Infof("Cluster name is %q and addon name is %q", o.ClusterName)

	// create kube client and shared informer factory for spoke cluster
	spokeKubeClient, err := kubernetes.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return err
	}
	spokeKubeInformerFactory := informers.NewSharedInformerFactory(spokeKubeClient, 10*time.Minute)

	// create hub clients and shared informer factories from hub kube config
	hubClientConfig, err := clientcmd.BuildConfigFromFlags("", o.HubKubeconfig)
	if err != nil {
		return err
	}
	hubKubeClient, err := kubernetes.NewForConfig(hubClientConfig)
	if err != nil {
		return err
	}
	hubKubeInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		hubKubeClient, 10*time.Minute,
		informers.WithTweakListOptions(
			func(listOptions *metav1.ListOptions) {
				listOptions.LabelSelector = fmt.Sprintf("open-cluster-management.io/addon-cluster-name=%s", o.ClusterName)
			},
		),
	)

	addonClient, err := addonclient.NewForConfig(hubClientConfig)
	if err != nil {
		return err
	}
	addonInformerFactory := addoninformers.NewSharedInformerFactory(addonClient, 10*time.Minute)

	// create another ClientCertForHubController for client certificate rotation
	clientCertForHubController := clientcertmanager.NewCertificateManagetController(
		addonClient,
		spokeKubeClient,
		hubClientConfig,
		hubKubeClient,
		addonInformerFactory.Addon().V1alpha1().ManagedClusterAddOns(),
		hubKubeInformerFactory.Core().V1().ConfigMaps(),
		spokeKubeInformerFactory.Core().V1().Secrets(),
		o.ComponentNamespace,
		o.ClusterName,
		controllerContext.EventRecorder,
	)

	addonLeaseController := lease.NewAddonLeaseController(
		o.ClusterName,
		addonClient,
		addonInformerFactory.Addon().V1alpha1().ManagedClusterAddOns(),
		spokeKubeInformerFactory.Coordination().V1().Leases(),
		1*time.Minute,
		controllerContext.EventRecorder,
	)

	hubLeaseUpdater := lease.NewLeaseUpdater(
		o.ClusterName,
		"addon-lease",
		hubKubeClient,
		controllerContext.EventRecorder,
	)

	go hubKubeInformerFactory.Start(ctx.Done())
	go spokeKubeInformerFactory.Start(ctx.Done())
	go addonInformerFactory.Start(ctx.Done())

	go clientCertForHubController.Run(ctx, 1)
	go addonLeaseController.Run(ctx, 1)
	go hubLeaseUpdater.Start(ctx, lease.AddonLeaseDurationSeconds)

	<-ctx.Done()
	return nil
}

// AddFlags registers flags for Agent
func (o *SpokeAgentOptions) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.ClusterName, "cluster-name", o.ClusterName,
		"Cluster name of the addon installed")
	fs.StringVar(&o.HubKubeconfig, "hub-kubeconfig", o.HubKubeconfig,
		"The mount path of hub-kubeconfig in the container.")
}

// Validate verifies the inputs.
func (o *SpokeAgentOptions) Validate() error {
	if o.ClusterName == "" {
		return errors.New("cluster name is empty")
	}

	return nil
}
