package lease

import (
	"context"
	"testing"
	"time"

	testinghelpers "github.com/open-cluster-management/addon-framework/pkg/helpers/testing"
	addonapiv1alpha1 "github.com/open-cluster-management/api/addon/v1alpha1"
	addonfake "github.com/open-cluster-management/api/client/addon/clientset/versioned/fake"
	addoninformers "github.com/open-cluster-management/api/client/addon/informers/externalversions"

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
		addons          []runtime.Object
		addonLeases     []runtime.Object
		validateActions func(t *testing.T, addonActions []clienttesting.Action)
	}{
		{
			name:        "there is no lease for an addon",
			addons:      []runtime.Object{testinghelpers.NewAddon()},
			addonLeases: []runtime.Object{},
			validateActions: func(t *testing.T, addonActions []clienttesting.Action) {
				expected := metav1.Condition{
					Type:    "Degraded",
					Status:  metav1.ConditionTrue,
					Reason:  "AddonLeaseNotFound",
					Message: "Addon agent is not found.",
				}
				testinghelpers.AssertActions(t, addonActions, "get", "update")
				actual := addonActions[1].(clienttesting.UpdateActionImpl).Object
				testinghelpers.AssertAddonCondition(t, actual.(*addonapiv1alpha1.ManagedClusterAddOn).Status.Conditions, expected)
			},
		},
		{
			name:        "addon agent stop update lease",
			addons:      []runtime.Object{testinghelpers.NewAddon()},
			addonLeases: []runtime.Object{testinghelpers.NewAddonLease(now.Add(-5*time.Minute), "ns1")},
			validateActions: func(t *testing.T, addonActions []clienttesting.Action) {
				expected := metav1.Condition{
					Type:    "Degraded",
					Status:  metav1.ConditionTrue,
					Reason:  "AddonLeaseUpdateStopped",
					Message: "Addon agent stopped updating its lease.",
				}
				testinghelpers.AssertActions(t, addonActions, "get", "update")
				actual := addonActions[1].(clienttesting.UpdateActionImpl).Object
				testinghelpers.AssertAddonCondition(t, actual.(*addonapiv1alpha1.ManagedClusterAddOn).Status.Conditions, expected)
			},
		},
		{
			name:        "addon agent is available",
			addons:      []runtime.Object{testinghelpers.NewAddon()},
			addonLeases: []runtime.Object{testinghelpers.NewAddonLease(now, "ns1")},
			validateActions: func(t *testing.T, addonActions []clienttesting.Action) {
				expected := metav1.Condition{
					Type:    "Degraded",
					Status:  metav1.ConditionFalse,
					Reason:  "ManagedClusterLeaseUpdated",
					Message: "Addon agent is updating its lease.",
				}
				testinghelpers.AssertActions(t, addonActions, "get", "update")
				actual := addonActions[1].(clienttesting.UpdateActionImpl).Object
				testinghelpers.AssertAddonCondition(t, actual.(*addonapiv1alpha1.ManagedClusterAddOn).Status.Conditions, expected)
			},
		},
		{
			name:        "multiple leases, one is available",
			addons:      []runtime.Object{testinghelpers.NewAddon()},
			addonLeases: []runtime.Object{testinghelpers.NewAddonLease(now, "ns1"), testinghelpers.NewAddonLease(now, "ns2")},
			validateActions: func(t *testing.T, addonActions []clienttesting.Action) {
				expected := metav1.Condition{
					Type:    "Degraded",
					Status:  metav1.ConditionFalse,
					Reason:  "ManagedClusterLeaseUpdated",
					Message: "Addon agent is updating its lease.",
				}
				testinghelpers.AssertActions(t, addonActions, "get", "update")
				actual := addonActions[1].(clienttesting.UpdateActionImpl).Object
				testinghelpers.AssertAddonCondition(t, actual.(*addonapiv1alpha1.ManagedClusterAddOn).Status.Conditions, expected)
			},
		},
		{
			name:   "multiple leases, none is available",
			addons: []runtime.Object{testinghelpers.NewAddon()},
			addonLeases: []runtime.Object{
				testinghelpers.NewAddonLease(now.Add(-5*time.Minute), "ns1"),
				testinghelpers.NewAddonLease(now.Add(-5*time.Minute), "ns2"),
			},
			validateActions: func(t *testing.T, addonActions []clienttesting.Action) {
				expected := metav1.Condition{
					Type:    "Degraded",
					Status:  metav1.ConditionTrue,
					Reason:  "AddonLeaseUpdateStopped",
					Message: "Addon agent stopped updating its lease.",
				}
				testinghelpers.AssertActions(t, addonActions, "get", "update")
				actual := addonActions[1].(clienttesting.UpdateActionImpl).Object
				testinghelpers.AssertAddonCondition(t, actual.(*addonapiv1alpha1.ManagedClusterAddOn).Status.Conditions, expected)
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
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

			ctrl := &addonLeaseController{
				clusterName: testinghelpers.TestManagedClusterName,
				addonClient: addonClient,
				addonLister: addonInformerFactory.Addon().V1alpha1().ManagedClusterAddOns().Lister(),
				leaseLister: leaseInformerFactory.Coordination().V1().Leases().Lister(),
			}
			syncErr := ctrl.sync(context.TODO(), testinghelpers.NewFakeSyncContext(t, ""))
			if syncErr != nil {
				t.Errorf("unexpected err: %v", syncErr)
			}
			c.validateActions(t, addonClient.Actions())
		})
	}
}
