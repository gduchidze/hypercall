package main

func snapshotChecks(profile string) []string {
	common := []string{"host-prerequisites", "cleanup", "log-redaction"}
	switch profile {
	case "phase1b-local":
		return append(common, "snapshot-boot", "checkpoint-capture", "capture-expiry", "capture-crash-recovery", "fork-continuity", "restored-clock", "restored-health-stability", "fork-isolation", "fork-authentication", "fork-retry", "artifact-rejection", "restore-crash-recovery")
	case "phase1b-capture":
		return append(common, "snapshot-boot", "checkpoint-capture", "cross-host-export")
	case "phase1b-restore":
		return append(common, "cross-host-source", "cross-host-restore", "restored-clock", "restored-health-stability", "restored-continuation", "restored-authentication")
	default:
		return nil
	}
}
