package cdd

import (
	"strconv"
	"strings"
)

func (resolver *resolver) precondition(node, template *element, bindingErr error) Precondition {
	condition := Precondition{
		MayBeExec:                    attributeValue(node, "mayBeExec"),
		NotExecInStateGroups:         attributeValue(node, "notExecInStateGroups"),
		TemplateMayBeExec:            attributeValue(template, "mayBeExec"),
		TemplateNotExecInStateGroups: attributeValue(template, "notExecInStateGroups"),
	}
	if bindingErr != nil {
		condition.Err = bindingErr
	} else {
		condition.Err = resolver.resolveStateList(&condition)
	}
	return condition
}

func (resolver *resolver) resolveStateList(condition *Precondition) error {
	if condition.NotExecInStateGroups != nil || condition.TemplateNotExecInStateGroups != nil {
		return sourceError(resolver.name, "notExecInStateGroups is not supported")
	}
	if condition.TemplateMayBeExec != nil {
		return sourceError(resolver.name, "template mayBeExec inheritance is not supported")
	}
	if condition.MayBeExec == nil {
		return nil
	}
	value := *condition.MayBeExec
	list := strings.TrimSpace(value)
	if len(list) < 3 || list[0] != '(' || list[len(list)-1] != ')' {
		return sourceError(resolver.name, "invalid mayBeExec list %q", value)
	}
	selected := make([]bool, len(resolver.states))
	for _, token := range strings.Split(list[1:len(list)-1], ",") {
		position, err := strconv.Atoi(strings.TrimSpace(token))
		if err != nil || position < 1 || position > len(resolver.states) {
			return sourceError(resolver.name, "mayBeExec %q names a state outside the %d declared states", value, len(resolver.states))
		}
		selected[position-1] = true
	}
	for index, state := range resolver.states {
		if !selected[index] {
			continue
		}
		condition.AllowedStates = append(condition.AllowedStates, state)
		switch state.GroupSpec {
		case "session":
			condition.Sessions = append(condition.Sessions, state)
		case "security":
			condition.SecurityLevels = append(condition.SecurityLevels, state)
		}
	}
	return nil
}

func attributeValue(node *element, name string) *string {
	if node == nil {
		return nil
	}
	value, present := node.attrs[name]
	if !present {
		return nil
	}
	return &value
}
