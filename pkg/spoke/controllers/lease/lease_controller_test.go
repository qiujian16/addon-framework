package lease

import (
	"context"
	"testing"
	"time"

	testinghelpers "github.com/open-cluster-management/addon-framework/pkg/helpers/testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestLeaseUpdate(t *testing.T) {
	cases := []struct {
		name            string
		validateActions func(t *testing.T, actions []clienttesting.Action)
		expectedErr     string
	}{
		{
			name: "start lease update routine",
			validateActions: func(t *testing.T, actions []clienttesting.Action) {
				testinghelpers.AssertUpdateActions(t, actions)
				leaseObj := actions[1].(clienttesting.UpdateActionImpl).Object
				lastLeaseObj := actions[len(actions)-1].(clienttesting.UpdateActionImpl).Object
				testinghelpers.AssertLeaseUpdated(t, leaseObj.(*coordinationv1.Lease), lastLeaseObj.(*coordinationv1.Lease))
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hubClient := kubefake.NewSimpleClientset(testinghelpers.NewManagedClusterAddonLease(time.Now()))
			clusterContext := testinghelpers.NewFakeSyncContext(t, "")

			leaseUpdater := NewLeaseUpdater(testinghelpers.TestManagedClusterName, "addon-lease", hubClient, clusterContext.Recorder())

			leaseUpdater.Start(context.TODO(), time.Duration(testinghelpers.TestLeaseDurationSeconds)*time.Second)
			// wait a few milliseconds to start the lease update routine
			time.Sleep(200 * time.Millisecond)

			// wait one cycle
			time.Sleep(1200 * time.Millisecond)
			c.validateActions(t, hubClient.Actions())
		})
	}
}
