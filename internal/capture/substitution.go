package capture

import "github.com/webvictim/prism/internal/usage"

// The cluster gateway rewrites a model name it does not serve to one it
// does, and tells nobody. Teleport's LLM proxy computes both names
// (`requestedModel` and `providerModel` in lib/srv/app/llm/anthropic)
// and then discards the pair: no response header, no audit field, and
// no reverse rewrite on the way back. Clients keep reporting the model
// they asked for — Claude Code prices a substituted call at the
// requested model's list rate — so a swapped request is
// indistinguishable from an honoured one unless something sits where
// both names are visible at once.
//
// prism is that something: the request body names one model and the
// response body names another, and the capture writer sees both.

// Substituted reports whether rec records a model the gateway swapped.
//
// RequestedModel is only populated when it differed, but the comparison
// is repeated here so a hand-built or hand-edited record — or one written
// before usage.SameModel learned about vendor prefixes — cannot claim a
// substitution that isn't one. usage.SameModel is the one definition of
// "the same model"; don't compare names any other way.
func Substituted(rec usage.Record) bool {
	return rec.RequestedModel != "" && rec.Model != "" &&
		!usage.SameModel(rec.RequestedModel, rec.Model)
}
