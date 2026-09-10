package cdd

import (
	"strconv"
	"strings"
)

func (resolver *resolver) transitions(node, template *element, bindingErr error) TransitionRule {
	rule := TransitionRule{
		RawExpression:         attributeValue(node, "trans"),
		RawTemplateExpression: attributeValue(template, "trans"),
	}
	switch {
	case bindingErr != nil:
		rule.Err = bindingErr
	case rule.RawTemplateExpression != nil:
		rule.Err = sourceError(resolver.name, "template trans inheritance is not supported")
	case rule.RawExpression != nil:
		rule.Pairs, rule.Err = resolver.transitionPairs(*rule.RawExpression)
	}
	return rule
}

func (resolver *resolver) transitionPairs(value string) ([]StateTransition, error) {
	// CANdelaStudio's State Transitions table assigns a destination to each
	// source state within a group (Vector manual 7.0, section 3.11.2). CDD trans
	// serialises those rows as a flat list of global source/destination indexes.
	list := strings.TrimSpace(value)
	if len(list) < 3 || list[0] != '(' || list[len(list)-1] != ')' {
		return nil, sourceError(resolver.name, "invalid trans list %q", value)
	}
	tokens := strings.Split(list[1:len(list)-1], ",")
	if len(tokens)%2 != 0 {
		return nil, sourceError(resolver.name, "trans %q must contain source/destination pairs", value)
	}
	states := make([]State, len(tokens))
	for index, token := range tokens {
		position, err := strconv.Atoi(strings.TrimSpace(token))
		if err != nil || position < 1 || position > len(resolver.states) {
			return nil, sourceError(resolver.name, "trans %q names a state outside the %d declared states", value, len(resolver.states))
		}
		states[index] = resolver.states[position-1]
	}
	pairs := make([]StateTransition, 0, len(states)/2)
	destinations := make(map[int]int)
	for index := 0; index < len(states); index += 2 {
		from, to := states[index], states[index+1]
		if from.GroupIndex != to.GroupIndex {
			return nil, sourceError(resolver.name, "trans %q crosses state groups from %d to %d", value, from.Index, to.Index)
		}
		if previous, exists := destinations[from.Index]; exists && previous != to.Index {
			return nil, sourceError(resolver.name, "trans %q assigns conflicting destinations to state %d", value, from.Index)
		}
		destinations[from.Index] = to.Index
		pairs = append(pairs, StateTransition{From: from, To: to})
	}
	return pairs, nil
}
