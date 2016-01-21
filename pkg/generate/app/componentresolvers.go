package app

import (
	"sort"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/util/errors"
)

// Resolver is an interface for resolving provided input to component matches.
// A Resolver should return ErrMultipleMatches when more than one result can
// be constructed as a match. It should also set the score to 0.0 if this is a
// perfect match, and to higher values the less adequate the match is.
type Resolver interface {
	Resolve(value string) (*ComponentMatch, error)
}

// Searcher is responsible for performing a search based on the given terms and return
// all results found as component matches. Notice they can even return zero or multiple
// matches, meaning they will never return ErrNoMatch or ErrMultipleMatches and any error
// returned is an actual error. The component match score can be used to determine how
// precise a given match is, where 0.0 is an exact match.
type Searcher interface {
	Search(terms ...string) (ComponentMatches, error)
}

// WeightedResolver is a resolver identified as exact or not, depending on its weight
type WeightedResolver struct {
	Searcher
	Weight float32
}

// PerfectMatchWeightedResolver returns only matches from resolvers that are identified as exact
// (weight 0.0), and only matches from those resolvers that qualify as exact (score = 0.0). If no
// perfect matches exist, an ErrMultipleMatches is returned indicating the remaining candidate(s).
// Note that this method may resolve ErrMultipleMatches with a single match, indicating an error
// (no perfect match) but with only one candidate.
type PerfectMatchWeightedResolver []WeightedResolver

// Resolve resolves the provided input and returns only exact matches
func (r PerfectMatchWeightedResolver) Resolve(value string) (*ComponentMatch, error) {
	imperfect := ScoredComponentMatches{}
	var group MultiSimpleSearcher
	var groupWeight float32 = 0.0
	for i, resolver := range r {
		// lump all resolvers with the same weight into a single group
		if len(group) == 0 || resolver.Weight == groupWeight {
			group = append(group, resolver.Searcher)
			groupWeight = resolver.Weight
			if i != len(r)-1 && r[i+1].Weight == groupWeight {
				continue
			}
		}
		matches, err := group.Search(value)
		switch {
		case len(matches) > 0:
			sort.Sort(ScoredComponentMatches(matches))
			if matches[0].Score == 0.0 && (len(matches) == 1 || matches[1].Score != 0.0) {
				return matches[0], nil
			}
			for _, m := range matches {
				if resolver.Weight != 0.0 {
					m.Score = resolver.Weight * m.Score
				}
				imperfect = append(imperfect, m)
			}
		case err != nil:
			glog.V(5).Infof("Error from resolver: %v\n", err)
			return nil, err
		}
		group = nil
	}

	switch len(imperfect) {
	case 0:
		// If value is a file and there is a TemplateFileSearcher in one of the resolvers
		// and trying to use it gives an error, use this error instead of ErrNoMatch.
		// E.g., calling `oc new-app template.json` where template.json is a file
		// with invalid JSON, it's better to return the JSON syntax error than a more
		// generic message.
		if isFile(value) {
			for _, resolver := range r {
				if _, ok := resolver.Searcher.(*TemplateFileSearcher); ok {
					if _, err := resolver.Search(value); err != nil {
						return nil, err
					}
				}
			}
		}
		return nil, ErrNoMatch{value: value}
	case 1:
		var err error
		if imperfect[0].Score != 0.0 {
			err = ErrPartialMatch{value, imperfect[0]}
		}
		return imperfect[0], err
	default:
		sort.Sort(imperfect)
		if imperfect[0].Score < imperfect[1].Score {
			var err error
			if imperfect[0].Score != 0.0 {
				err = ErrPartialMatch{value, imperfect[0]}
			}
			return imperfect[0], err
		}
		return nil, ErrMultipleMatches{value, imperfect}
	}
}

// FirstMatchResolver simply takes the first search result returned by the
// searcher it holds and resolves it to that match. An ErrMultipleMatches will
// never happen given it will just take the first result, but a ErrNoMatch can
// happen if the searcher returns no matches.
type FirstMatchResolver struct {
	Searcher Searcher
}

// Resolve resolves as the first match returned by the Searcher
func (r FirstMatchResolver) Resolve(value string) (*ComponentMatch, error) {
	matches, err := r.Searcher.Search(value)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, ErrNoMatch{value: value}
	}
	return matches[0], nil
}

// HighestScoreResolver takes search result returned by the searcher it holds
// and resolves it to the highest scored match present. An ErrMultipleMatches
// will never happen given it will just take the best scored result, but a
// ErrNoMatch can happen if the searcher returns no matches.
type HighestScoreResolver struct {
	Searcher Searcher
}

// Resolve resolves as the first highest scored match returned by the Searcher
func (r HighestScoreResolver) Resolve(value string) (*ComponentMatch, error) {
	matches, err := r.Searcher.Search(value)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, ErrNoMatch{value: value}
	}
	sort.Sort(ScoredComponentMatches(matches))
	return matches[0], nil
}

// HighestUniqueScoreResolver takes search result returned by the searcher it
// holds and resolves it to the highest scored match present. If more than one
// match exists with that same score, returns an ErrMultipleMatches. A ErrNoMatch
// can happen if the searcher returns no matches.
type HighestUniqueScoreResolver struct {
	Searcher Searcher
}

// Resolve resolves as the highest scored match returned by the Searcher, and
// guarantees the match is unique (the only match with that given score)
func (r HighestUniqueScoreResolver) Resolve(value string) (*ComponentMatch, error) {
	matches, err := r.Searcher.Search(value)
	if err != nil {
		return nil, err
	}
	sort.Sort(ScoredComponentMatches(matches))
	switch len(matches) {
	case 0:
		return nil, ErrNoMatch{value: value}
	case 1:
		return matches[0], nil
	default:
		if matches[0].Score == matches[1].Score {
			return nil, ErrMultipleMatches{value, matches}
		}
		return matches[0], nil
	}
}

// UniqueExactOrInexactMatchResolver takes search result returned by the searcher
// it holds. Returns the single exact match present, if more that one exact match
// is present, returns a ErrMultipleMatches. If no exact match is present, try with
// inexact ones, which must also be unique otherwise ErrMultipleMatches. A ErrNoMatch
// can happen if the searcher returns no exact or inexact matches.
type UniqueExactOrInexactMatchResolver struct {
	Searcher Searcher
}

// Resolve resolves as the single exact or inexact match present
func (r UniqueExactOrInexactMatchResolver) Resolve(value string) (*ComponentMatch, error) {
	matches, err := r.Searcher.Search(value)
	if err != nil {
		return nil, err
	}
	sort.Sort(ScoredComponentMatches(matches))

	exact := matches.Exact()
	switch len(exact) {
	case 0:
		inexact := matches.Inexact()
		switch len(inexact) {
		case 0:
			return nil, ErrNoMatch{value: value}
		case 1:
			return inexact[0], nil
		default:
			return nil, ErrMultipleMatches{value, exact}
		}
	case 1:
		return exact[0], nil
	default:
		return nil, ErrMultipleMatches{value, exact}
	}
}

// MultiSimpleSearcher is a set of searchers
type MultiSimpleSearcher []Searcher

// Search searches using all searchers it holds
func (s MultiSimpleSearcher) Search(terms ...string) (ComponentMatches, error) {
	var errs []error
	componentMatches := ComponentMatches{}
	for _, searcher := range s {
		matches, err := searcher.Search(terms...)
		if err != nil {
			glog.V(2).Infof("Error occurred during search: %s", err)
			errs = append(errs, err)
			continue
		}
		componentMatches = append(componentMatches, matches...)
	}
	sort.Sort(ScoredComponentMatches(componentMatches))
	return componentMatches, errors.NewAggregate(errs)
}

// WeightedSearcher is a searcher identified as exact or not, depending on its weight
type WeightedSearcher struct {
	Searcher
	Weight float32
}

// MultiWeightedSearcher is a set of weighted searchers where lower weight has higher
// priority in search results
type MultiWeightedSearcher []WeightedSearcher

// Search searches using all searchers it holds and score according to searcher height
func (s MultiWeightedSearcher) Search(terms ...string) (ComponentMatches, error) {
	componentMatches := ComponentMatches{}
	for _, searcher := range s {
		matches, err := searcher.Search(terms...)
		if err != nil {
			glog.V(2).Infof("Error occurred during search: %#v", err)
			continue
		}
		for _, match := range matches {
			match.Score += searcher.Weight
			componentMatches = append(componentMatches, match)
		}
	}
	sort.Sort(ScoredComponentMatches(componentMatches))
	return componentMatches, nil
}

func searchExact(searcher Searcher, value string) (exact *ComponentMatch, inexact []*ComponentMatch, err error) {
	matches, err := searcher.Search(value)
	if err != nil {
		return nil, nil, err
	}

	exactMatches := matches.Exact()
	inexactMatches := matches.Inexact()
	switch len(exactMatches) {
	case 0:
		return nil, inexactMatches, nil
	case 1:
		return exactMatches[0], inexactMatches, nil
	default:
		return nil, nil, ErrMultipleMatches{value, exactMatches}
	}
}
