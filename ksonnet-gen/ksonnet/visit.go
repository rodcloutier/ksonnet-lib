package ksonnet

import (
	"bytes"
	"fmt"
	"log"
	"strings"

	"github.com/ksonnet/ksonnet-lib/ksonnet-gen/jsonnet"
	"github.com/ksonnet/ksonnet-lib/ksonnet-gen/kubespec"
)

// Visitor is the interface for a Ksonnet gen visitor
type Visitor interface {
	visitRoot(root *root)
	visitGroup(group *group, hidden bool)
	visitVersionedAPI(va *versionedAPI)
	visitComments(cs *comments)

	visitAPIObject(ao *apiObject)
	visitRefMixinAPIObject(rmao *refMixinAPIObject)

	visitConstructor(c *constructor)
	visitProperty(p *property)
	visitTypeAlias(p *property)
	visitRefProperty(p *property)
}

type emitVisitor struct {
	m            *indentWriter
	groups       bytes.Buffer
	hiddenGroups bytes.Buffer

	buffers map[interface{}]*bytes.Buffer
}

func newEmitVisitor(iw *indentWriter) *emitVisitor {

	e := emitVisitor{
		m:       iw,
		buffers: make(map[interface{}]*bytes.Buffer),
	}

	return &e
}

func (e *emitVisitor) visitRoot(root *root) {
	e.m.writeLine("// AUTOGENERATED from the Kubernetes OpenAPI specification. DO NOT MODIFY.")
	e.m.writeLine(fmt.Sprintf("// Kubernetes version: %s", root.spec.Info.Version))

	if root.ksonnetLibSHA != nil {
		e.m.writeLine(fmt.Sprintf(
			"// SHA of ksonnet-lib HEAD: %s", *root.ksonnetLibSHA))
	}

	if root.ksonnetLibSHA != nil {
		e.m.writeLine(fmt.Sprintf(
			"// SHA of Kubernetes HEAD OpenAPI spec is generated from: %s",
			*root.k8sSHA))
	}
	e.m.writeLine("")

	e.m.writeLine("{")
	e.m.indent()

	e.m.write(e.groups)

	e.m.writeLine("local hidden = {")
	e.m.indent()

	e.m.write(e.hiddenGroups)

	e.m.dedent()
	e.m.writeLine("},")

	e.m.dedent()
	e.m.writeLine("}")
}

func (e *emitVisitor) visitGroup(group *group, hidden bool) {

	buffer := &e.groups
	if hidden {
		buffer = &e.hiddenGroups
	}

	k8sVersion := group.root().spec.Info.Version
	mixinName := jsonnet.RewriteAsIdentifier(k8sVersion, group.name)
	line := fmt.Sprintf("%s:: {", mixinName)

	e.m.bufferWriteLine(buffer, line)
	e.m.indent()

	for _, versioned := range group.versionedAPIs.toSortedSlice() {
		buffer.Write(e.buffers[versioned].Bytes())
	}

	e.m.dedent()
	e.m.bufferWriteLine(buffer, "},")
}

func (e *emitVisitor) visitVersionedAPI(va *versionedAPI) {

	buffer := bytes.Buffer{}
	e.buffers[va] = &buffer

	// NOTE: Do not need to call `jsonnet.RewriteAsIdentifier`.
	line := fmt.Sprintf("%s:: {", va.version)
	e.m.bufferWriteLine(&buffer, line)
	e.m.indent()

	gn := va.parent.qualifiedName
	if gn == "core" {
		e.m.bufferWriteLine(&buffer, fmt.Sprintf(
			"local apiVersion = {apiVersion: \"%s\"},", va.version))
	} else {
		e.m.bufferWriteLine(&buffer, fmt.Sprintf(
			"local apiVersion = {apiVersion: \"%s/%s\"},", gn, va.version))
	}

	// Emit in sorted order so that we can diff the output.
	for _, object := range va.apiObjects.toSortedSlice() {
		buffer.Write(e.buffers[object].Bytes())
	}

	e.m.dedent()
	e.m.bufferWriteLine(&buffer, "},")
}

func (e *emitVisitor) visitAPIObject(ao *apiObject) {

	buffer := bytes.Buffer{}
	e.buffers[ao] = &buffer

	k8sVersion := ao.root().spec.Info.Version
	jsonnetName := kubespec.ObjectKind(jsonnet.RewriteAsIdentifier(k8sVersion, ao.name))

	buffer.Write(e.buffers[&ao.comments].Bytes())

	e.m.bufferWriteLine(&buffer, fmt.Sprintf("%s:: {", jsonnetName))
	e.m.indent()

	if ao.isTopLevel {
		// NOTE: It is important to NOT capitalize `ao.name` here.
		e.m.bufferWriteLine(&buffer, fmt.Sprintf("local kind = {kind: \"%s\"},", ao.name))
	}

	for _, constructor := range ao.constructors {
		buffer.Write(e.buffers[constructor].Bytes())
	}

	for _, pm := range ao.properties.sortAndFilterBlacklisted() {
		if isSpecialProperty(pm.name) || isMixinRef(pm.ref) {
			continue
		}
		buffer.Write(e.buffers[pm].Bytes())
	}

	// Emit the properties that `$ref` another API object type in the
	// `mixin:: {` namespace.
	e.m.bufferWriteLine(&buffer, "mixin:: {")
	e.m.indent()

	for _, pm := range ao.properties.sortAndFilterBlacklisted() {
		// TODO: Emit mixin code also for arrays whose elements are
		// `$ref`.
		if !isMixinRef(pm.ref) {
			continue
		}

		buffer.Write(e.buffers[pm].Bytes())
	}

	e.m.dedent()
	e.m.bufferWriteLine(&buffer, "},")

	e.m.dedent()
	e.m.bufferWriteLine(&buffer, "},")
}

func (e *emitVisitor) visitRefMixinAPIObject(rmao *refMixinAPIObject) {

	p := rmao.prop

	buffer := bytes.Buffer{}
	e.buffers[p] = &buffer

	k8sVersion := rmao.root().spec.Info.Version
	functionName := jsonnet.RewriteAsIdentifier(k8sVersion, p.name)
	paramName := jsonnet.RewriteAsFuncParam(k8sVersion, p.name)
	fieldName := jsonnet.RewriteAsFieldKey(p.name)
	mixinName := fmt.Sprintf("__%sMixin", functionName)
	var mixinText string
	if rmao.parentMixinName == "" {
		mixinText = fmt.Sprintf(
			"local %s(%s) = {%s+: %s},", mixinName, paramName, fieldName, paramName)
	} else {
		mixinText = fmt.Sprintf(
			"local %s(%s) = %s({%s+: %s}),",
			mixinName, paramName, rmao.parentMixinName, fieldName, paramName)
	}

	if _, ok := rmao.parent.apiObjects[kubespec.ObjectKind(functionName)]; ok {
		log.Panicf(
			"Tried to lowercase first character of object kind '%s', but lowercase name was already present in version '%s'",
			functionName,
			rmao.parent.version)
	}

	// NOTE: Comments are emitted by `property#emit`, before we
	// call this method.

	line := fmt.Sprintf("%s:: {", functionName)
	e.m.bufferWriteLine(&buffer, line)
	e.m.indent()

	e.m.bufferWriteLine(&buffer, mixinText)
	e.m.bufferWriteLine(&buffer,
		fmt.Sprintf("mixinInstance(%s):: %s(%s),", paramName, mixinName, paramName))

	for _, pm := range rmao.properties.sortAndFilterBlacklisted() {
		if pbuffer, ok := e.buffers[pm]; ok {
			buffer.Write(pbuffer.Bytes())
		}
	}

	e.m.dedent()
	e.m.bufferWriteLine(&buffer, "},")
}

func (e *emitVisitor) visitConstructor(c *constructor) {
	// Build parameters and body of constructor. Considering the example
	// of the constructor of `v1.Container`:
	//
	//   new(name, image):: self.name(name) + self.image(image),
	//
	// Here we want to (1) assemble the parameter list (i.e., `name` and
	// `image`), as well as the body (i.e., the calls to `self.name` and
	// so on).
	paramLiterals := []string{}
	setters := c.defaultSetters
	for _, param := range c.params {
		// Add the param to the param list, including default value if
		// applicable.
		if param.DefaultValue != nil {
			paramLiterals = append(
				paramLiterals, fmt.Sprintf("%s=%s", param.ID, *param.DefaultValue))
		} else {
			paramLiterals = append(paramLiterals, param.ID)
		}

		// Add an element to the body (e.g., `self.name` above).
		if param.RelativePath == nil {
			prop, ok := c.apiObj.properties[kubespec.PropertyName(param.ID)]
			if !ok {
				log.Panicf(
					"Attempted to create constructor, but property '%s' does not exist",
					param.ID)
			}
			k8sVersion := c.apiObj.root().spec.Info.Version
			propMethodName := jsonnet.RewriteAsIdentifier(k8sVersion, prop.name).ToSetterID()
			setters = append(
				setters, fmt.Sprintf("self.%s(%s)", propMethodName, param.ID))
		} else {
			// TODO(hausdorff): We may want to verify this relative path
			// exists.
			setters = append(
				setters, fmt.Sprintf("self.%s(%s)", *param.RelativePath, param.ID))
		}
	}

	// Write out constructor.
	paramsText := strings.Join(paramLiterals, ", ")
	bodyText := strings.Join(setters, " + ")
	buffer := bytes.Buffer{}
	e.buffers[c] = &buffer
	e.m.bufferWriteLine(&buffer, fmt.Sprintf("%s(%s):: %s,", c.specName, paramsText, bodyText))
}

func (e *emitVisitor) visitProperty(p *property) {

	buffer := bytes.Buffer{}
	e.buffers[p] = &buffer

	//paramType := *p.schemaType

	////
	//// Generate both setter and mixin functions for some property. For
	//// example, we emit both `metadata.setAnnotations({foo: "bar"})`
	//// (which replaces a set of annotations with given object) and
	//// `metadata.mixinAnnotations({foo: "bar"})` (which replaces only
	//// the `foo` key, if it exists.)
	////

	//var setterBody string
	//var mixinBody string
	//emitMixin := false
	//switch paramType {
	//case "array":
	//	emitMixin = true
	//	if parentMixinName == nil {
	//		setterBody = fmt.Sprintf(
	//			"if std.type(%s) == \"array\" then {%s: %s} else {%s: [%s]}",
	//			paramName, fieldName, paramName, fieldName, paramName,
	//		)
	//		mixinBody = fmt.Sprintf(
	//			"if std.type(%s) == \"array\" then {%s+: %s} else {%s+: [%s]}",
	//			paramName, fieldName, paramName, fieldName, paramName,
	//		)
	//	} else {
	//		setterBody = fmt.Sprintf(
	//			"if std.type(%s) == \"array\" then %s({%s: %s}) else %s({%s: [%s]})",
	//			paramName, *parentMixinName, fieldName, paramName, *parentMixinName,
	//			fieldName, paramName,
	//		)
	//		mixinBody = fmt.Sprintf(
	//			"if std.type(%s) == \"array\" then %s({%s+: %s}) else %s({%s+: [%s]})",
	//			paramName, *parentMixinName, fieldName, paramName, *parentMixinName,
	//			fieldName, paramName,
	//		)
	//	}
	//case "integer", "string", "boolean":
	//	if parentMixinName == nil {
	//		setterBody = fmt.Sprintf("{%s: %s}", fieldName, paramName)
	//	} else {
	//		setterBody = fmt.Sprintf("%s({%s: %s})", *parentMixinName, fieldName, paramName)
	//	}
	//case "object":
	//	emitMixin = true
	//	if parentMixinName == nil {
	//		setterBody = fmt.Sprintf("{%s: %s}", fieldName, paramName)
	//		mixinBody = fmt.Sprintf("{%s+: %s}", fieldName, paramName)
	//	} else {
	//		setterBody = fmt.Sprintf("%s({%s: %s})", *parentMixinName, fieldName, paramName)
	//		mixinBody = fmt.Sprintf("%s({%s+: %s})", *parentMixinName, fieldName, paramName)
	//	}
	//default:
	//	log.Panicf("Unrecognized type '%s'", paramType)
	//}

	////
	//// Emit.
	////

	//line := fmt.Sprintf("%s self + %s,", setterSignature, setterBody)
	//m.writeLine(line)

	//if emitMixin {
	//	// TODO [rod] p.comments.emit(m)
	//	line = fmt.Sprintf("%s self + %s,", mixinSignature, mixinBody)
	//	m.writeLine(line)
	//}
}

func (e *emitVisitor) visitTypeAlias(p *property) {

	buffer := bytes.Buffer{}
	e.buffers[p] = &buffer

	var path kubespec.DefinitionName
	if p.ref != nil {
		path = *p.ref.Name()
	} else {
		path = *p.itemTypes.Ref.Name()
	}
	parsedPath := path.Parse()
	if parsedPath.Version == nil {
		log.Printf("Could not emit type alias for '%s'\n", path)
		return
	}

	// Chop the `Type` off the end of the type alias name, rewrite the
	// "base" of the type alias, and then append `Type` to the end
	// again.
	//
	// Why: the desired behavior is for a rewrite rule to apply to both
	// a method and its type alias. For example, if we specify that
	// `scaleIO` should be rewritten `scaleIo`, then we'd like the type
	// alias to be emitted as `scaleIoType`, not `scaleIOType`,
	// automatically, so that the user doesn't have to specify another,
	// separate rule for the type alias itself.
	k8sVersion := p.root().spec.Info.Version
	trimmedName := kubespec.PropertyName(strings.TrimSuffix(string(p.name), "Type"))
	typeName := jsonnet.RewriteAsIdentifier(k8sVersion, trimmedName) + "Type"

	var group kubespec.GroupName
	if parsedPath.Group == nil {
		group = "core"
	} else {
		group = *parsedPath.Group
	}

	id := jsonnet.RewriteAsIdentifier(k8sVersion, parsedPath.Kind)
	line := fmt.Sprintf(
		"%s:: hidden.%s.%s.%s,",
		typeName, group, parsedPath.Version, id)

	e.m.bufferWriteLine(&buffer, line)
}

func (e *emitVisitor) visitRefProperty(p *property) {

	buffer := bytes.Buffer{}
	e.buffers[p] = &buffer

	k8sVersion := p.root().spec.Info.Version
	setterFunctionName := jsonnet.RewriteAsIdentifier(k8sVersion, p.name).ToSetterID()
	fieldName := jsonnet.RewriteAsFieldKey(p.name)
	paramName := jsonnet.RewriteAsFuncParam(k8sVersion, p.name)
	setterSignature := fmt.Sprintf("%s(%s)::", setterFunctionName, paramName)

	var body string
	if p.parentMixinName == "" {
		body = fmt.Sprintf("{%s: %s}", fieldName, paramName)
	} else {
		body = fmt.Sprintf("%s({%s: %s})", p.parentMixinName, fieldName, paramName)
	}
	line := fmt.Sprintf("%s %s,", setterSignature, body)

	e.m.bufferWriteLine(&buffer, line)
}

func (e *emitVisitor) visitComments(cs *comments) {

	buffer := bytes.Buffer{}
	e.buffers[cs] = &buffer

	for _, comment := range *cs {
		if comment == "" {
			// Don't create trailing space if comment is empty.
			e.m.bufferWriteLine(&buffer, "//")
		} else {
			e.m.bufferWriteLine(&buffer, fmt.Sprintf("// %s", comment))
		}
	}
}
