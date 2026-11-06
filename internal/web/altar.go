package web

import (
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
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

// returnMark is a spirit whose latest death is a return to a tombstone
// (Requirement 18.2): its number on that tombstone and the tombstone's photo.
type returnMark struct {
	N      int
	URL    string
	DiedAt time.Time
	// Self is set when the spirit is the enshrined Pod itself, not another
	// member of its family. TombPod is the enshrined Pod.
	Self    bool
	TombPod string
	Family  string
}

// altarVisit is what the spirit list knows about the altar: the enshrined
// spirits and the spirits that returned. A nil *altarVisit knows nothing.
type altarVisit struct {
	shrine  enshrined
	returns map[string]returnMark
}

// lookup returns the tombstone ID of a spirit on the altar.
func (v *altarVisit) lookup(namespace, pod string) (string, bool) {
	if v == nil {
		return "", false
	}
	return v.shrine.lookup(namespace, pod)
}

// returnOf returns the return mark of a spirit.
func (v *altarVisit) returnOf(namespace, pod string) (returnMark, bool) {
	if v == nil {
		return returnMark{}, false
	}
	m, ok := v.returns[enshrinedKey(namespace, pod)]
	return m, ok
}

// visitAltar reads the altar for the spirit list and records each return
// (Requirement 18.2): a spirit is matched to a tombstone with graveyard.Match,
// and a death after the burial is stored with Store.Return. Only the local
// altar file is written. With the graveyard off or unreadable nothing is
// enshrined and nothing returns: the list then fades as usual, which is the
// safe side, and the error is logged.
func (s *Server) visitAltar(spirits []autopsy.SpiritSummary) *altarVisit {
	if s.opts.Graveyard == nil {
		return nil
	}
	all, err := s.opts.Graveyard.List()
	if err != nil {
		s.opts.Logger.Printf("read altar for the spirit list: %v", err)
		return nil
	}
	v := &altarVisit{shrine: enshrinedOf(all), returns: map[string]returnMark{}}
	for _, sp := range spirits {
		t, ok := graveyard.Match(all, sp.Namespace, sp.Name, sp.Family)
		if !ok {
			continue
		}
		if sp.LastDeath != nil && t.Counts(sp.Name, *sp.LastDeath) {
			saved, added, err := s.opts.Graveyard.Return(t.ID, graveyard.Return{Pod: sp.Name, DiedAt: *sp.LastDeath, Cause: sp.Cause})
			if err != nil {
				s.opts.Logger.Printf("record the return of %s/%s: %v", sp.Namespace, sp.Name, err)
			} else if added {
				t = saved
				for i := range all {
					if all[i].ID == t.ID {
						all[i] = t
					}
				}
			}
		}
		if n, r, ok := t.LastReturnOf(sp.Name); ok {
			v.returns[enshrinedKey(sp.Namespace, sp.Name)] = returnMark{
				N: n, URL: tombstoneURL(t.ID), DiedAt: r.DiedAt,
				Self: sp.Name == t.PodName, TombPod: t.PodName, Family: t.Family,
			}
		}
	}
	return v
}
