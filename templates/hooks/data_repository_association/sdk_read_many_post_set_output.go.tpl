	// FAILED is documented as unrecoverable; surface it as Terminal with
	// FSx's FailureDetails rather than requeueing forever.
	if err := terminalIfFailed(ko); err != nil {
		return &resource{ko}, err
	}
