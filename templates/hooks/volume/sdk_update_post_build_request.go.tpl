	// UpdateVolume has no Tags member, so tags are synced with
	// TagResource/UntagResource and, when they are the only change, the
	// update call is skipped.
	// Runs after the generated `updateable.when` guard, so tags are not synced
	// while the volume is in a held state.
	if delta.DifferentAt("Spec.Tags") {
		if err = rm.syncTags(ctx, desired, latest); err != nil {
			return nil, err
		}
	}
	if !delta.DifferentExcept("Spec.Tags") {
		return desired, nil
	}
