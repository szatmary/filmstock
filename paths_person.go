package filmstock

import "fmt"

// PersonRecordPathID is FNV-1a over a wiki link target, used only to give a
// person with no Q-id a stable path. It is never an identity — the identity is
// the link target itself, which is stored in the record.
func PersonRecordPathID(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h & 0x7fffffff
}

// personShardPath is the shard-relative path of a person record, the same
// function RecordPath applies, exposed so a Location can be built without a
// record root.
func personShardPath(id int64) string {
	shard := id % shardCount
	if shard < 0 {
		shard = -shard
	}
	return fmt.Sprintf("%02x/%d.json.gz", shard, id)
}
