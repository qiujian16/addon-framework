package helpers

import (
	"context"

	addonapiv1alpha1 "github.com/open-cluster-management/api/addon/v1alpha1"
	addonv1alpha1client "github.com/open-cluster-management/api/client/addon/clientset/versioned"
	workapiv1 "github.com/open-cluster-management/api/work/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

func ManifestsEqual(new, old []workapiv1.Manifest) bool {
	if len(new) != len(old) {
		return false
	}

	for i := range new {
		if !equality.Semantic.DeepEqual(new[i].Raw, old[i].Raw) {
			return false
		}
	}
	return true
}

type UpdateAddonStatusFunc func(status *addonapiv1alpha1.ManagedClusterAddOnStatus) error

func UpdateAddonStatus(
	ctx context.Context,
	client addonv1alpha1client.Interface,
	spokeClusterName string,
	addonName string,
	updateFuncs ...UpdateAddonStatusFunc) (*addonapiv1alpha1.ManagedClusterAddOnStatus, bool, error) {
	updated := false
	var updatedManagedClusterStatus *addonapiv1alpha1.ManagedClusterAddOnStatus

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		addon, err := client.AddonV1alpha1().ManagedClusterAddOns(spokeClusterName).Get(ctx, addonName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		oldStatus := &addon.Status

		newStatus := oldStatus.DeepCopy()
		for _, update := range updateFuncs {
			if err := update(newStatus); err != nil {
				return err
			}
		}
		if equality.Semantic.DeepEqual(oldStatus, newStatus) {
			// We return the newStatus which is a deep copy of oldStatus but with all update funcs applied.
			updatedManagedClusterStatus = newStatus
			return nil
		}

		addon.Status = *newStatus
		updatedManagedCluster, err := client.AddonV1alpha1().ManagedClusterAddOns(spokeClusterName).UpdateStatus(ctx, addon, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		updatedManagedClusterStatus = &updatedManagedCluster.Status
		updated = err == nil
		return err
	})

	return updatedManagedClusterStatus, updated, err
}

func UpdateAddonConditionFn(cond metav1.Condition) UpdateAddonStatusFunc {
	return func(oldStatus *addonapiv1alpha1.ManagedClusterAddOnStatus) error {
		meta.SetStatusCondition(&oldStatus.Conditions, cond)
		return nil
	}
}
