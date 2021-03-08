package lease

import (
	"context"
	"testing"
	"time"

	testinghelpers "github.com/open-cluster-management/addon-framework/pkg/helpers/testing"
	addonapiv1alpha1 "github.com/open-cluster-management/api/addon/v1alpha1"
	addonfake "github.com/open-cluster-management/api/client/addon/clientset/versioned/fake"
	addoninformers "github.com/open-cluster-management/api/client/addon/informers/externalversions"
	clusterfake "github.com/open-cluster-management/api/client/cluster/clientset/versioned/fake"
	clusterinformers "github.com/open-cluster-management/api/client/cluster/informers/externalversions"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubeinformers "k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

var now = time.Now()

func TestSync(t *testing.T) {
	cases := []struct {
		name            string
		clusters        []runtime.Object
		addons          []runtime.Object
		addonLeases     []runtime.Object
		validateActions func(t *testing.T, leaseActions, addonActions []clienttesting.Action)
	}{
		{
			name:        "sync managed cluster",
			clusters:    []runtime.Object{testinghelpers.NewManagedCluster()},
			addonLeases: []runtime.Object{},
			validateActions: func(t *testing.T, leaseActions, addonActions []clienttesting.Action) {
				testinghelpers.AssertActions(t, leaseActions, "create")
				testinghelpers.AssertNoActions(t, addonActions)
			},
		},
		{
			name:        "managed cluster stop update lease",
			clusters:    []runtime.Object{testinghelpers.NewManagedCluster()},
			addons:      []runtime.Object{testinghelpers.NewAddon()},
			addonLeases: []runtime.Object{testinghelpers.NewAddonManagerLease(now.Add(-5 * time.Minute))},
			validateActions: func(t *testing.T, leaseActions, addonActions []clienttesting.Action) {
				expected := metav1.Condition{
					Type:    "Available",
					Status:  metav1.ConditionUnknown,
					Reason:  "AddonManagerUpdateStopped",
					Message: "Addon manager stopped updating its lease.",
				}
				testinghelpers.AssertActions(t, addonActions, "get", "update")
				actual := addonActions[1].(clienttesting.UpdateActionImpl).Object
				testinghelpers.AssertAddonCondition(t, actual.(*addonapiv1alpha1.ManagedClusterAddOn).Status.Conditions, expected)
			},
		},
		{
			name:        "managed cluster is available",
			clusters:    []runtime.Object{testinghelpers.NewManagedCluster()},
			addons:      []runtime.Object{testinghelpers.NewAddon()},
			addonLeases: []runtime.Object{testinghelpers.NewAddonManagerLease(now)},
			validateActions: func(t *testing.T, leaseActions, addonActions []clienttesting.Action) {
				testinghelpers.AssertNoActions(t, addonActions)
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clusterClient := clusterfake.NewSimpleClientset(c.clusters...)
			clusterInformerFactory := clusterinformers.NewSharedInformerFactory(clusterClient, time.Minute*10)
			clusterStore := clusterInformerFactory.Cluster().V1().ManagedClusters().Informer().GetStore()
			for _, cluster := range c.clusters {
				clusterStore.Add(cluster)
			}

			addonClient := addonfake.NewSimpleClientset(c.addons...)
			addonInformerFactory := addoninformers.NewSharedInformerFactory(addonClient, time.Minute*10)
			addonStore := addonInformerFactory.Addon().V1alpha1().ManagedClusterAddOns().Informer().GetStore()
			for _, addon := range c.addons {
				addonStore.Add(addon)
			}

			leaseClient := kubefake.NewSimpleClientset(c.addonLeases...)
			leaseInformerFactory := kubeinformers.NewSharedInformerFactory(leaseClient, time.Minute*10)
			leaseStore := leaseInformerFactory.Coordination().V1().Leases().Informer().GetStore()
			for _, lease := range c.addonLeases {
				leaseStore.Add(lease)
			}

			ctrl := &leaseController{
				kubeClient:    leaseClient,
				addonClient:   addonClient,
				clusterLister: clusterInformerFactory.Cluster().V1().ManagedClusters().Lister(),
				leaseLister:   leaseInformerFactory.Coordination().V1().Leases().Lister(),
				addonLister:   addonInformerFactory.Addon().V1alpha1().ManagedClusterAddOns().Lister(),
			}
			syncErr := ctrl.sync(context.TODO(), testinghelpers.NewFakeSyncContext(t, ""))
			if syncErr != nil {
				t.Errorf("unexpected err: %v", syncErr)
			}
			c.validateActions(t, leaseClient.Actions(), addonClient.Actions())
		})
	}
}
