	// Describe has no ConstituentsPerAggregate and reports a conditional
	// CoolingPeriod default; keep what the user submitted.
	restoreNonRoundTrippedSpecFields(r.ko, ko)
	// FAILED is final; surface it as Terminal with FSx's
	// LifecycleTransitionReason rather than requeueing forever. Checked
	// before the tag read so a failing ListTagsForResource cannot hide it.
	if err := terminalIfFailed(ko); err != nil {
		keepDesiredTags(r.ko, ko)
		return &resource{ko}, err
	}
	// DescribeVolumes does not report volume tags; read them from
	// ListTagsForResource (skipped during deletion, keeping desired's).
	if err := rm.setTagsFromAWS(ctx, r.ko, ko); err != nil {
		return nil, err
	}
