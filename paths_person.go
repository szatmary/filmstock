package filmstock

// PersonRecordPathID is FNV-1a over a wiki link target, used only to give a
// person with no Q-id and no page_id a stable key. It is not an identity — the
// identity is the link target itself, which is stored in the record.
//
// It is 64-bit because 31 was demonstrably too few. Two exports of identical
// input put "Issa Abdessamie" and "Costache Ciubotaru" under the same key, and
// gitdb keys are unique, so one of them was simply not in the database. At
// 77,457 redlinked credits a 31-bit space expects about 1.4 such collisions and
// grows with the SQUARE of the count: 150k redlinks expects ~5, 300k expects
// ~21. Every one is a person silently replaced by a stranger.
//
// A 63-bit space expects 3e-10 collisions at the same 77,457, and still about
// 1e-8 at ten times that. The bound is masked to 63 bits rather than 64 because
// the caller negates it to keep these keys apart from page_ids, and int64 has
// no room for the sign bit otherwise.
//
// This makes the key sound. It does not make it canonical: it is still derived
// from a display string, so it changes if the article is ever created, and two
// genuinely different people credited under one name still share it. Only
// dropping these records, or keying them on something the encyclopaedia states,
// fixes that.
func PersonRecordPathID(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h & 0x7fffffffffffffff
}
