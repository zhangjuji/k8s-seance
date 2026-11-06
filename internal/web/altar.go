package web

import (
	"github.com/zhangjuji/k8s-seance/internal/graveyard"
)

// The ofrenda (Requirement 15). The graveyard page is drawn as a Día de los
// Muertos altar: papel picado, stepped tiers of framed photos, candles and a
// marigold petal path, all in CSS. Spirits whose postmortem is on the altar are
// "enshrined": the list no longer fades them or raises the forgetting banner,
// because their evidence is kept in the postmortem.

// altarTierSizes is how many photos the first tiers hold. Later tiers hold as
// many as the last entry.
var altarTierSizes = []int{3, 4, 5}

// altarTier is one step of the altar. Level counts from 1 at the top.
type altarTier struct {
	Level  int
	Stones []stoneView
}

// altarTiers cuts stones, the most recent death first, into tiers of 3, 4, 5,
// 5, … photos. The top tier holds the most recent deaths. No stones, no tiers.
func altarTiers(stones []stoneView) []altarTier {
	var tiers []altarTier
	for i := 0; len(stones) > 0; i++ {
		n := altarTierSizes[min(i, len(altarTierSizes)-1)]
		n = min(n, len(stones))
		tiers = append(tiers, altarTier{Level: i + 1, Stones: stones[:n:n]})
		stones = stones[n:]
	}
	return tiers
}

// enshrined maps "namespace/pod" to the tombstone ID of every spirit on the altar.
type enshrined map[string]string

func enshrinedKey(namespace, pod string) string { return namespace + "/" + pod }

// lookup returns the tombstone ID of a spirit on the altar.
func (e enshrined) lookup(namespace, pod string) (string, bool) {
	id, ok := e[enshrinedKey(namespace, pod)]
	return id, ok
}

// enshrinedOf builds the set from tombstones.
func enshrinedOf(all []graveyard.Tombstone) enshrined {
	e := make(enshrined, len(all))
	for _, t := range all {
		k := enshrinedKey(t.Namespace, t.PodName)
		if _, dup := e[k]; !dup {
			e[k] = t.ID
		}
	}
	return e
}

// enshrinedSpirits reads the altar for the spirit list. With the graveyard off
// or unreadable nothing is enshrined: the list then fades as usual, which is the
// safe side, and the error is logged.
func (s *Server) enshrinedSpirits() enshrined {
	if s.opts.Graveyard == nil {
		return nil
	}
	all, err := s.opts.Graveyard.List()
	if err != nil {
		s.opts.Logger.Printf("read altar for the spirit list: %v", err)
		return nil
	}
	return enshrinedOf(all)
}
