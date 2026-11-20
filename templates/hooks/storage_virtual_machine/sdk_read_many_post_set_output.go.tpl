	// Describe never returns the AD join Password; keep desired's secret
	// reference so the CR does not lose it.
	restoreADPassword(r.ko, ko)
	// Create-only and not always reported by Describe; keep the declared
	// value when Describe returns none.
	restoreRootVolumeSecurityStyle(r.ko, ko)
	// Describe does not report SVM tags; read them from ListTagsForResource.
	if err := rm.setTagsFromAWS(ctx, ko); err != nil {
		return nil, err
	}
	// FAILED is final; surface it as Terminal with FSx's
	// LifecycleTransitionReason rather than requeueing forever.
	if err := terminalIfFailed(ko); err != nil {
		return &resource{ko}, err
	}
