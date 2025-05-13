package server

type Decision int

const (
	Allow Decision = iota
	Block
	Rewrote
	NoMatch
	Kill
	Slow
)

type DecisionResult struct {
	Decision Decision
	Reason   string
}

type Decider interface {
	Decide(packet *InspectorPacket) DecisionResult
}

type RuleBasedDecider struct {
	Rules []Rule
}

type Rule struct {
	Name        string
	Description string
	Matcher     func(packet *InspectorPacket) bool
	Action      Decision
	Reason      string
}

// Decide implements the Decider interface
func (d *RuleBasedDecider) Decide(packet *InspectorPacket) DecisionResult {
	for _, rule := range d.Rules {
		if rule.Matcher(packet) && rule.Action != Rewrote {
			return DecisionResult{
				Decision: rule.Action,
				Reason:   rule.Reason,
			}
		}
	}
	return DecisionResult{Decision: NoMatch, Reason: "No matching rule found"}
}

func NewRuleBasedDecider(rules []Rule) *RuleBasedDecider {
	return &RuleBasedDecider{Rules: rules}
}
