package internal

// Per-seed (per Brave client_id) cap on the number of stored passwords.
//
// Rather than rejecting commits ourselves, we drive go-sync's own per-client
// object quota (command.go: maxClientObjectQuota). Before every create, the
// upstream commit handler checks
//
//	ItemCount + <creates so far in this commit> >= maxClientObjectQuota
//
// and answers that entry with CommitResponse_OVER_QUOTA, while still letting
// updates and deletes through. That quota is hard-coded upstream, so
// GetClientItemCount reports a shifted count:
//
//	ItemCount = livePasswords + (upstreamMaxClientObjectQuota - maxPasswords)
//
// which makes the upstream check trip exactly when a seed would exceed
// maxPasswords. The client gets a real OVER_QUOTA per entity instead of an
// HTTP error that would stall the whole commit.
//
// TestPasswordQuota_* pins this behaviour against the upstream handler, so a
// go-sync bump that changes the quota constant or its maths fails loudly.
const (
	// upstreamMaxClientObjectQuota mirrors the unexported
	// maxClientObjectQuota in github.com/brave/go-sync/command.
	upstreamMaxClientObjectQuota = 50000

	// defaultMaxPasswords is the default number of live (non-deleted)
	// passwords a single seed may store.
	defaultMaxPasswords = 120

	// passwordDataType is the EntitySpecifics field number for passwords,
	// which go-sync stores as the entity's data_type.
	passwordDataType = 45873
)

// loadMaxPasswords reads LITESYNC_MAX_PASSWORDS. A negative value disables the
// password cap (the upstream 50k object quota then applies on real counts).
func loadMaxPasswords() int {
	return envInt("LITESYNC_MAX_PASSWORDS", defaultMaxPasswords)
}

// quotaItemCount translates live counts into the ItemCount reported to the
// upstream quota check.
func quotaItemCount(maxPasswords, livePasswords, liveTotal int) int {
	if maxPasswords < 0 {
		return liveTotal
	}
	return livePasswords + (upstreamMaxClientObjectQuota - maxPasswords)
}
