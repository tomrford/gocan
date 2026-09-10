package cdd

import (
	"errors"
	"fmt"
	"strconv"
)

type resolver struct {
	name      string
	ecuDoc    *element
	byID      map[string]*element
	datatypes map[string]*element
	states    []State
}

func newResolver(name string, ecuDoc *element) *resolver {
	resolver := &resolver{name: name, ecuDoc: ecuDoc, byID: make(map[string]*element), datatypes: make(map[string]*element)}
	resolver.index(ecuDoc)
	if datatypes := ecuDoc.child("DATATYPES"); datatypes != nil {
		for _, datatype := range datatypes.children {
			if id := datatype.attr("id"); id != "" {
				resolver.datatypes[id] = resolver.byID[id]
			}
		}
	}
	return resolver
}

func (resolver *resolver) index(node *element) {
	if id := node.attr("id"); id != "" {
		if _, exists := resolver.byID[id]; exists {
			// A duplicate ID cannot prove a reference. Keep it ambiguous instead
			// of selecting whichever definition happened to appear last.
			resolver.byID[id] = nil
		} else {
			resolver.byID[id] = node
		}
	}
	for _, child := range node.children {
		resolver.index(child)
	}
}

func (resolver *resolver) reference(id, kind string) (*element, error) {
	node, exists := resolver.byID[id]
	if !exists {
		return nil, sourceError(resolver.name, "%s reference %q does not resolve", kind, id)
	}
	if node == nil {
		return nil, sourceError(resolver.name, "%s reference %q is ambiguous (duplicate XML id)", kind, id)
	}
	if node.name != kind {
		return nil, sourceError(resolver.name, "reference %q points to %s, want %s", id, node.name, kind)
	}
	return node, nil
}

func metadataPointer(node *element) *Metadata {
	if node == nil {
		return nil
	}
	value := metadata(node)
	return &value
}

func (resolver *resolver) resolve(ecu, variant *element, selection Selection) *Database {
	database := &Database{
		Selection: selection, ECU: metadata(ecu), Variant: metadata(variant),
		didsByName: make(map[string]int), didsByIdentifier: make(map[uint16]int),
	}
	for groupIndex, group := range resolver.ecuDoc.child("STATEGROUPS").childrenNamed("STATEGROUP") {
		for _, node := range group.childrenNamed("STATE") {
			state := State{Metadata: metadata(node), Index: len(database.States) + 1,
				Group: metadata(group), GroupIndex: groupIndex + 1, GroupSpec: group.attr("spec")}
			database.States = append(database.States, state)
			switch state.GroupSpec {
			case "session":
				database.Sessions = append(database.Sessions, state)
			case "security":
				database.SecurityLevels = append(database.SecurityLevels, state)
			}
		}
	}
	resolver.states = database.States
	rootPath := fmt.Sprintf("ECU[%d]/VAR[%d]", selection.ECU, selection.Variant)
	var classIndex, instanceIndex int
	for _, node := range variant.children {
		switch node.name {
		case "DIAGCLASS":
			classIndex++
			path := fmt.Sprintf("%s/DIAGCLASS[%d]", rootPath, classIndex)
			class := &Class{Metadata: metadata(node), TemplateRef: node.attr("tmplref")}
			template, err := resolver.reference(class.TemplateRef, "DCLTMPL")
			class.Template, class.Err = metadataPointer(template), err
			database.Entries = append(database.Entries, Entry{Class: class})
			database.report(path, class.Source, DiagnosticReference, err)
			for index, child := range node.childrenNamed("DIAGINST") {
				instancePath := fmt.Sprintf("%s/DIAGINST[%d]", path, index+1)
				class.Instances = append(class.Instances, resolver.resolveInstance(database, instancePath, child))
			}
		case "DIAGINST":
			instanceIndex++
			path := fmt.Sprintf("%s/DIAGINST[%d]", rootPath, instanceIndex)
			database.Entries = append(database.Entries, Entry{Instance: resolver.resolveInstance(database, path, node)})
		}
	}

	return database
}

func (resolver *resolver) resolveInstance(database *Database, path string, node *element) *Instance {
	instance := &Instance{Metadata: metadata(node), TemplateRef: node.attr("tmplref")}
	template, err := resolver.reference(instance.TemplateRef, "DCLTMPL")
	instance.Template, instance.Err = metadataPointer(template), err
	database.report(path, instance.Source, DiagnosticReference, err)
	for index, serviceNode := range node.childrenNamed("SERVICE") {
		servicePath := fmt.Sprintf("%s/SERVICE[%d]", path, index+1)
		service := resolver.resolveService(node, template, serviceNode, index+1)
		instance.Services = append(instance.Services, service)
		if service.Protocol == nil {
			database.report(servicePath, service.Source, DiagnosticReference, service.Err)
		} else {
			// The binding resolved; a missing/invalid SID is a request defect.
			database.report(servicePath+"/REQ", service.Source, DiagnosticMessage, service.Err)
		}
		database.report(servicePath, service.Source, DiagnosticPrecondition, service.Requirements.Err)
		database.report(servicePath, service.Source, DiagnosticTransition, service.Transitions.Err)
		for _, entry := range []struct {
			name    string
			message *Message
		}{{"REQ", service.Request}, {"POS", service.PositiveResponse}} {
			if entry.message == nil {
				continue
			}
			messagePath := servicePath + "/" + entry.name
			database.report(messagePath, service.Source, DiagnosticMessage, entry.message.Err)
			if entry.message.Record != nil {
				database.report(messagePath, service.Source, DiagnosticCodec, entry.message.Record.CodecError())
			}
		}
	}
	database.collectDIDs(path, instance)
	return instance
}

func (database *Database) report(path string, source SourceIdentity, kind DiagnosticKind, err error) {
	if err != nil {
		database.Diagnostics = append(database.Diagnostics, Diagnostic{Path: path, Source: source, Kind: kind, Message: err.Error()})
	}
}

func (resolver *resolver) resolveService(instance, classTemplate, node *element, index int) *Service {
	service := &Service{Metadata: metadata(node), Index: index, TemplateRef: node.attr("tmplref")}
	template, err := resolver.reference(service.TemplateRef, "DCLSRVTMPL")
	service.Template = metadataPointer(template)
	if err == nil && (classTemplate == nil || !directChild(classTemplate, template)) {
		err = sourceError(resolver.name, "service template %q is not a child of the instance's DCLTMPL %q", service.TemplateRef, instance.attr("tmplref"))
	}
	// Literal rules depend on this template, not on protocol-message support.
	service.Requirements = resolver.precondition(node, template, err)
	service.Transitions = resolver.transitions(node, template, err)
	var protocol *element
	if err == nil {
		service.ProtocolRef = template.attr("tmplref")
		protocol, err = resolver.reference(service.ProtocolRef, "PROTOCOLSERVICE")
		service.Protocol = metadataPointer(protocol)
	}
	service.Err = err
	if err != nil {
		return service
	}
	service.Request = resolver.resolveMessage(instance, classTemplate, protocol.child("REQ"))
	service.PositiveResponse = resolver.resolveMessage(instance, classTemplate, protocol.child("POS"))
	service.ServiceID, service.Err = serviceID(service.Request)
	return service
}

func serviceID(request *Message) (*uint8, error) {
	if request == nil {
		return nil, fmt.Errorf("protocol service has no REQ")
	}
	var sid *uint8
	var count int
	for _, parameter := range request.Parameters {
		if parameter.Spec != "sid" {
			continue
		}
		count++
		if parameter.Err == nil && parameter.BitLength == 8 && parameter.NumericValue != nil {
			value := uint8(*parameter.NumericValue)
			sid = &value
		}
	}
	if count != 1 || sid == nil {
		return nil, fmt.Errorf("request does not declare exactly one resolved 8-bit service ID")
	}
	return sid, nil
}

func (resolver *resolver) resolveMessage(instance, classTemplate, node *element) *Message {
	if node == nil {
		return nil
	}
	message := &Message{Metadata: metadata(node)}
	var problems []error
	var dataComponents []*element
	for _, component := range node.children {
		switch component.name {
		case "NAME", "QUAL", "DESC":
		case "CONSTCOMP", "STATICCOMP":
			parameter := resolver.resolveParameter(instance, classTemplate, component)
			message.Parameters = append(message.Parameters, parameter)
			if parameter.Err != nil {
				problems = append(problems, parameter.Err)
			}
		case "SIMPLEPROXYCOMP":
			if component.attr("dest") == "data" {
				dataComponents = append(dataComponents, component)
			} else {
				problems = append(problems, sourceError(resolver.name, "unsupported SIMPLEPROXYCOMP destination %q", component.attr("dest")))
			}
		default:
			problems = append(problems, sourceError(resolver.name, "message contains unsupported %s", component.name))
		}
	}
	if len(dataComponents) > 1 {
		problems = append(problems, sourceError(resolver.name, "message contains multiple data components"))
	} else if len(dataComponents) == 1 {
		component := dataComponents[0]
		// Resolve a single component independently for each source service.
		if _, err := resolver.reference(component.attr("id"), "SIMPLEPROXYCOMP"); err != nil {
			problems = append(problems, err)
		} else {
			var err error
			message.Record, err = resolver.resolveRecord(instance.childText("QUAL"), instance, classTemplate, component.attr("id"))
			if err != nil {
				problems = append(problems, err)
			}
		}
	}
	message.Err = errors.Join(problems...)
	return message
}

func (resolver *resolver) resolveParameter(instance, classTemplate, component *element) Parameter {
	parameter := Parameter{Metadata: metadata(component), Spec: component.attr("spec")}
	if component.name == "STATICCOMP" || component.attr("id") != "" {
		_, parameter.Err = resolver.reference(component.attr("id"), component.name)
	}
	width := component.attr("bl")
	if reference := component.attr("dtref"); reference != "" {
		datatype := resolver.datatypes[reference]
		if datatype == nil {
			parameter.Err = errors.Join(parameter.Err, sourceError(resolver.name, "parameter %q references unknown or ambiguous datatype %q", component.attr("id"), reference))
		} else {
			parameter.Datatype = metadataPointer(datatype)
			width = datatype.child("CVALUETYPE").attr("bl")
		}
	}
	if component.name == "CONSTCOMP" {
		parameter.Value = attributeValue(component, "v")
	} else {
		var statics []*element
		for _, static := range classTemplate.childrenNamed("SHSTATIC") {
			for _, reference := range static.childrenNamed("STATICCOMPREF") {
				if reference.attr("idref") == component.attr("id") {
					statics = append(statics, static)
					break
				}
			}
		}
		if len(statics) != 1 {
			parameter.Err = errors.Join(parameter.Err, sourceError(resolver.name, "STATICCOMP %q has %d SHSTATIC bindings, want one", component.attr("id"), len(statics)))
		} else {
			static := statics[0]
			parameter.Static = metadataPointer(static)
			_, referenceErr := resolver.reference(static.attr("id"), "SHSTATIC")
			parameter.Err = errors.Join(parameter.Err, referenceErr)
			var values []*element
			for _, value := range instance.childrenNamed("STATICVALUE") {
				if value.attr("shstaticref") == static.attr("id") {
					values = append(values, value)
				}
			}
			if len(values) == 1 {
				parameter.Value = attributeValue(values[0], "v")
			} else {
				parameter.Err = errors.Join(parameter.Err, sourceError(resolver.name, "SHSTATIC %q has %d instance values, want one", static.attr("id"), len(values)))
			}
		}
	}
	bits, err := strconv.ParseUint(width, 10, 32)
	if err != nil || bits == 0 || bits > 64 {
		parameter.Err = errors.Join(parameter.Err, sourceError(resolver.name, "parameter %q has unsupported width %q", component.attr("id"), width))
		return parameter
	}
	parameter.BitLength = uint32(bits)
	if parameter.Value == nil {
		parameter.Err = errors.Join(parameter.Err, sourceError(resolver.name, "parameter %q has no value", component.attr("id")))
	} else if value, err := strconv.ParseUint(*parameter.Value, 10, int(bits)); err != nil {
		parameter.Err = errors.Join(parameter.Err, sourceError(resolver.name, "parameter %q has invalid %d-bit unsigned value %q", component.attr("id"), bits, *parameter.Value))
	} else if parameter.Err == nil {
		parameter.NumericValue = &value
	}
	return parameter
}

func (database *Database) collectDIDs(path string, instance *Instance) {
	byIdentifier := make(map[uint16]*DID)
	for _, service := range instance.Services {
		if service.Err != nil || service.ServiceID == nil || *service.ServiceID != 0x22 && *service.ServiceID != 0x2e {
			continue
		}
		identifier, err := didIdentifier(service)
		if err != nil {
			database.report(fmt.Sprintf("%s/SERVICE[%d]", path, service.Index), service.Source, DiagnosticDID, err)
			continue
		}
		did := byIdentifier[*identifier]
		if did == nil {
			did = &DID{Instance: instance, Identifier: *identifier}
			byIdentifier[*identifier] = did
			index := len(database.DIDs)
			database.DIDs = append(database.DIDs, did)
			name := instance.Source.Qualifier
			if name != "" {
				if _, exists := database.didsByName[name]; exists {
					database.didsByName[name] = -1
					database.report(path, instance.Source, DiagnosticDID, fmt.Errorf("DID qualifier %q is ambiguous", name))
				} else {
					database.didsByName[name] = index
				}
			}
			if _, exists := database.didsByIdentifier[did.Identifier]; exists {
				database.didsByIdentifier[did.Identifier] = -1
				database.report(path, instance.Source, DiagnosticDID, fmt.Errorf("DID identifier %#04x is ambiguous", did.Identifier))
			} else {
				database.didsByIdentifier[did.Identifier] = index
			}
		}
		if *service.ServiceID == 0x22 {
			did.Read = append(did.Read, service)
		} else {
			did.Write = append(did.Write, service)
		}
	}
}

func directChild(parent, candidate *element) bool {
	for _, child := range parent.children {
		if child == candidate {
			return true
		}
	}
	return false
}

// A DID may bind its identifier in REQ, POS, or both. If both declare it, they
// must agree. This view describes the binding, not complete message encodability.
func didIdentifier(service *Service) (*uint16, error) {
	var identifier *uint16
	for _, message := range []*Message{service.Request, service.PositiveResponse} {
		if message == nil {
			continue
		}
		var count int
		for _, parameter := range message.Parameters {
			if parameter.Spec != "id" {
				continue
			}
			count++
			if count > 1 || parameter.Err != nil || parameter.BitLength != 16 || parameter.NumericValue == nil {
				return nil, fmt.Errorf("read/write-by-identifier message does not prove one 16-bit identifier")
			}
			value := uint16(*parameter.NumericValue)
			if identifier != nil && *identifier != value {
				return nil, fmt.Errorf("request and response DID identifiers disagree")
			}
			identifier = &value
		}
	}
	if identifier == nil {
		return nil, fmt.Errorf("read/write-by-identifier service has no 16-bit identifier")
	}
	return identifier, nil
}
