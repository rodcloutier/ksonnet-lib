package ksonnet

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/ksonnet/ksonnet-lib/ksonnet-gen/jsonnet"
	"github.com/ksonnet/ksonnet-lib/ksonnet-gen/kubespec"
	"github.com/ksonnet/ksonnet-lib/ksonnet-gen/kubeversion"
)

// Emit takes a swagger API specification, and returns the text of
// `ksonnet-lib`, written in Jsonnet.
func Emit(spec *kubespec.APISpec, ksonnetLibSHA, k8sSHA *string) ([]byte, []byte, error) {
	root := newRoot(spec, ksonnetLibSHA, k8sSHA)

	m := newIndentWriter()
	/*
		root.emit(m)
		/*/
	emitVisitor := newEmitVisitor(m)
	root.accept(emitVisitor)
	//*/

	k8sBytes, err := m.bytes()
	if err != nil {
		return nil, nil, err
	}

	kBytes := []byte(kubeversion.KSource(spec.Info.Version))

	return kBytes, k8sBytes, nil
}

//-----------------------------------------------------------------------------
// Root.
//-----------------------------------------------------------------------------

// `root` is an abstraction of the root of `k8s.libsonnet`, which can be
// emitted as Jsonnet code using the `emit` method.
//
// `root` contains and manages a set of `groups`, which represent a
// set of Kubernetes API groups (e.g., core, apps, extensions), and
// holds all of the logic required to build the `groups` from an
// `kubespec.APISpec`.
type root struct {
	spec         *kubespec.APISpec
	groups       groupSet // set of groups, e.g., core, apps, extensions.
	hiddenGroups groupSet

	ksonnetLibSHA *string
	k8sSHA        *string
}

func newRoot(spec *kubespec.APISpec, ksonnetLibSHA, k8sSHA *string) *root {
	root := root{
		spec:         spec,
		groups:       make(groupSet),
		hiddenGroups: make(groupSet),

		ksonnetLibSHA: ksonnetLibSHA,
		k8sSHA:        k8sSHA,
	}

	for defName, def := range spec.Definitions {
		root.addDefinition(defName, def)
	}

	return &root
}

func (root *root) accept(visitor Visitor) {

	for _, group := range root.groups.toSortedSlice() {
		group.accept(visitor, false)
	}

	for _, hiddenGroup := range root.hiddenGroups.toSortedSlice() {
		hiddenGroup.accept(visitor, true)
	}

	visitor.visitRoot(root)
}

func (root *root) emit(m *indentWriter) {
	m.writeLine("// AUTOGENERATED from the Kubernetes OpenAPI specification. DO NOT MODIFY.")
	m.writeLine(fmt.Sprintf("// Kubernetes version: %s", root.spec.Info.Version))

	if root.ksonnetLibSHA != nil {
		m.writeLine(fmt.Sprintf(
			"// SHA of ksonnet-lib HEAD: %s", *root.ksonnetLibSHA))
	}

	if root.ksonnetLibSHA != nil {
		m.writeLine(fmt.Sprintf(
			"// SHA of Kubernetes HEAD OpenAPI spec is generated from: %s",
			*root.k8sSHA))
	}
	m.writeLine("")

	m.writeLine("{")
	m.indent()

	// Emit in sorted order so that we can diff the output.
	for _, group := range root.groups.toSortedSlice() {
		group.emit(m)
	}

	m.writeLine("local hidden = {")
	m.indent()

	for _, hiddenGroup := range root.hiddenGroups.toSortedSlice() {
		hiddenGroup.emit(m)
	}

	m.dedent()
	m.writeLine("},")

	m.dedent()
	m.writeLine("}")
}

func (root *root) addDefinition(
	path kubespec.DefinitionName, def *kubespec.SchemaDefinition,
) {
	parsedName := path.Parse()
	if parsedName.Version == nil {
		return
	}
	apiObject := root.createAPIObject(parsedName, def)

	for propName, prop := range def.Properties {
		pm := newPropertyMethod(propName, path, prop, apiObject)
		apiObject.properties[propName] = pm

		st := prop.Type
		if isMixinRef(pm.ref) ||
			(st != nil && *st == "array" && prop.Items.Ref != nil) {
			typeAliasName := propName + "Type"
			ta, ok := apiObject.properties[typeAliasName]
			if ok && ta.kind != typeAlias {
				log.Panicf(
					"Can't create type alias '%s' because a property with that name already exists", typeAliasName)
			}

			ta = newPropertyTypeAlias(typeAliasName, path, prop, apiObject)
			apiObject.properties[typeAliasName] = ta
		}
	}
}

func (root *root) createAPIObject(
	parsedName *kubespec.ParsedDefinitionName, def *kubespec.SchemaDefinition,
) *apiObject {
	if parsedName.Version == nil {
		log.Panicf(
			"Can't make API object from name with nil version in path: '%s'",
			parsedName.Unparse())
	}

	var groupName kubespec.GroupName
	if parsedName.Group == nil {
		groupName = "core"
	} else {
		groupName = *parsedName.Group
	}

	var qualifiedName kubespec.GroupName
	if len(def.TopLevelSpecs) > 0 && def.TopLevelSpecs[0].Group != "" {
		qualifiedName = def.TopLevelSpecs[0].Group
	} else {
		qualifiedName = groupName
	}

	// Separate out top-level definitions from everything else.
	var groups groupSet
	if len(def.TopLevelSpecs) > 0 {
		groups = root.groups
	} else {
		groups = root.hiddenGroups
	}

	group, ok := groups[groupName]
	if !ok {
		group = newGroup(groupName, qualifiedName, root)
		groups[groupName] = group
	}

	versionedAPI, ok := group.versionedAPIs[*parsedName.Version]
	if !ok {
		versionedAPI = newVersionedAPI(*parsedName.Version, group)
		group.versionedAPIs[*parsedName.Version] = versionedAPI
	}

	apiObject, ok := versionedAPI.apiObjects[parsedName.Kind]
	if ok {
		log.Panicf("Duplicate object kinds with name '%s'", parsedName.Unparse())
	}
	apiObject = newAPIObject(parsedName, versionedAPI, def)
	versionedAPI.apiObjects[parsedName.Kind] = apiObject
	return apiObject
}

func (root *root) getAPIObject(
	parsedName *kubespec.ParsedDefinitionName,
) *apiObject {
	ao, err := root.getAPIObjectHelper(parsedName, false)
	if err == nil {
		return ao
	}

	ao, err = root.getAPIObjectHelper(parsedName, true)
	if err != nil {
		log.Panic(err.Error())
	}
	return ao
}

func (root *root) getAPIObjectHelper(
	parsedName *kubespec.ParsedDefinitionName, hidden bool,
) (*apiObject, error) {
	if parsedName.Version == nil {
		log.Panicf(
			"Can't get API object with nil version: '%s'", parsedName.Unparse())
	}

	var groupName kubespec.GroupName
	if parsedName.Group == nil {
		groupName = "core"
	} else {
		groupName = *parsedName.Group
	}

	var groups groupSet
	if hidden {
		groups = root.groups
	} else {
		groups = root.hiddenGroups
	}

	group, ok := groups[groupName]
	if !ok {
		return nil, fmt.Errorf(
			"Could not retrieve object, group in path '%s' doesn't exist",
			parsedName.Unparse())
	}

	versionedAPI, ok := group.versionedAPIs[*parsedName.Version]
	if !ok {
		return nil, fmt.Errorf(
			"Could not retrieve object, versioned API in path '%s' doesn't exist",
			parsedName.Unparse())
	}

	if apiObject, ok := versionedAPI.apiObjects[parsedName.Kind]; ok {
		return apiObject, nil
	}
	return nil, fmt.Errorf(
		"Could not retrieve object, kind in path '%s' doesn't exist",
		parsedName.Unparse())
}

//-----------------------------------------------------------------------------
// Group.
//-----------------------------------------------------------------------------

// `group` is an abstract representation of a Kubernetes API group
// (e.g., apps, extensions, core), which can be emitted as Jsonnet
// code using the `emit` method.
//
// `group` contains a set of versioned APIs (e.g., v1, v1beta1, etc.),
// though the logic for creating them is handled largely by `root`.
type group struct {
	name          kubespec.GroupName // e.g., core, apps, extensions.
	qualifiedName kubespec.GroupName // e.g., rbac.authorization.k8s.io.
	versionedAPIs versionedAPISet    // e.g., v1, v1beta1.
	parent        *root
}
type groupSet map[kubespec.GroupName]*group
type groupSlice []*group

func newGroup(
	name kubespec.GroupName, qualifiedName kubespec.GroupName, parent *root,
) *group {
	return &group{
		name:          name,
		qualifiedName: qualifiedName,
		versionedAPIs: make(versionedAPISet),
		parent:        parent,
	}
}

func (group *group) root() *root {
	return group.parent
}

func (group *group) accept(visitor Visitor, hidden bool) {
	for _, versioned := range group.versionedAPIs.toSortedSlice() {
		versioned.accept(visitor)
	}
	visitor.visitGroup(group, hidden)
}

func (group *group) emit(m *indentWriter) {
	k8sVersion := group.root().spec.Info.Version
	mixinName := jsonnet.RewriteAsIdentifier(k8sVersion, group.name)
	line := fmt.Sprintf("%s:: {", mixinName)
	m.writeLine(line)
	m.indent()

	// Emit in sorted order so that we can diff the output.
	for _, versioned := range group.versionedAPIs.toSortedSlice() {
		versioned.emit(m)
	}

	m.dedent()
	m.writeLine("},")
}

func (gs groupSet) toSortedSlice() groupSlice {
	groups := groupSlice{}
	for _, group := range gs {
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].name < groups[j].name
	})
	return groups
}

//-----------------------------------------------------------------------------
// Versioned API.
//-----------------------------------------------------------------------------

// `versionedAPI` is an abstract representation of a version of a
// Kubernetes API group (e.g., apps.v1beta1, extensions.v1beta1,
// core.v1), which can be emitted as Jsonnet code using the `emit`
// method.
//
// `versionedAPI` contains a set of API objects (e.g., v1.Container,
// v1beta1.Deployment, etc.), though the logic for creating them is
// handled largely by `root`.
type versionedAPI struct {
	version    kubespec.VersionString // version string, e.g., v1, v1beta1.
	apiObjects apiObjectSet           // set of objects, e.g, v1.Container.
	parent     *group
}
type versionedAPISet map[kubespec.VersionString]*versionedAPI
type versionedAPISlice []*versionedAPI

func newVersionedAPI(
	version kubespec.VersionString, parent *group,
) *versionedAPI {
	return &versionedAPI{
		version:    version,
		apiObjects: make(apiObjectSet),
		parent:     parent,
	}
}

func (va *versionedAPI) root() *root {
	return va.parent.parent
}

func (va *versionedAPI) accept(visitor Visitor) {
	for _, object := range va.apiObjects.toSortedSlice() {
		object.accept(visitor)
	}
	visitor.visitVersionedAPI(va)
}

func (va *versionedAPI) emit(m *indentWriter) {
	// NOTE: Do not need to call `jsonnet.RewriteAsIdentifier`.
	line := fmt.Sprintf("%s:: {", va.version)
	m.writeLine(line)
	m.indent()

	gn := va.parent.qualifiedName
	if gn == "core" {
		m.writeLine(fmt.Sprintf(
			"local apiVersion = {apiVersion: \"%s\"},", va.version))
	} else {
		m.writeLine(fmt.Sprintf(
			"local apiVersion = {apiVersion: \"%s/%s\"},", gn, va.version))
	}

	// Emit in sorted order so that we can diff the output.
	for _, object := range va.apiObjects.toSortedSlice() {
		object.emit(m)
	}

	m.dedent()
	m.writeLine("},")
}

func (vas versionedAPISet) toSortedSlice() versionedAPISlice {
	versionedAPIs := versionedAPISlice{}
	for _, va := range vas {
		versionedAPIs = append(versionedAPIs, va)
	}
	sort.Slice(versionedAPIs, func(i, j int) bool {
		return versionedAPIs[i].version < versionedAPIs[j].version
	})
	return versionedAPIs
}

//-----------------------------------------------------------------------------
// Constructor object
//-----------------------------------------------------------------------------

type constructor struct {
	specName kubespec.PropertyName
}

func newConstructor(id string, params []kubeversion.CustomConstructorParam) *constructor {

	return &constructor{
		specName: kubespec.PropertyName(id),
	}
}

//-----------------------------------------------------------------------------
// API object.
//-----------------------------------------------------------------------------

// `apiObject` is an abstract representation of a Kubernetes API
// object (e.g., v1.Container, v1beta1.Deployment), which can be
// emitted as Jsonnet code using the `emit` method.
//
// `apiObject` contains a set of property methods and mixins which
// formulate the basis of much of ksonnet-lib's programming surface.
// The logic for creating them is handled largely by `root`.
type apiObject struct {
	name       kubespec.ObjectKind // e.g., `Container` in `v1.Container`
	properties propertySet         // e.g., container.image, container.env
	parsedName *kubespec.ParsedDefinitionName
	comments   comments
	parent     *versionedAPI
	isTopLevel bool
}
type apiObjectSet map[kubespec.ObjectKind]*apiObject
type apiObjectSlice []*apiObject

func newAPIObject(
	name *kubespec.ParsedDefinitionName, parent *versionedAPI,
	def *kubespec.SchemaDefinition,
) *apiObject {
	isTopLevel := len(def.TopLevelSpecs) > 0
	comments := newComments(def.Description)
	return &apiObject{
		name:       name.Kind,
		parsedName: name,
		properties: make(propertySet),
		comments:   comments,
		parent:     parent,
		isTopLevel: isTopLevel,
	}
}

func (ao apiObject) toRefPropertyMethod(
	name kubespec.PropertyName, path kubespec.DefinitionName, parent *apiObject,
) *property {
	return &property{
		ref:        path.AsObjectRef(),
		schemaType: nil,
		itemTypes:  kubespec.Items{},
		name:       name,
		path:       path,
		comments:   ao.comments,
		parent:     parent,
	}
}

func (ao *apiObject) root() *root {
	return ao.parent.parent.parent
}

func (ao *apiObject) accept(visitor Visitor) {

	ao.comments.accept(visitor)

	// TODO [rod] emitConstructors

	for _, pm := range ao.properties.sortAndFilterBlacklisted() {
		pm.accept(visitor)
	}

	visitor.visitAPIObject(ao)
}

func (ao *apiObject) emit(m *indentWriter) {
	k8sVersion := ao.root().spec.Info.Version
	jsonnetName := kubespec.ObjectKind(
		jsonnet.RewriteAsIdentifier(k8sVersion, ao.name))
	if _, ok := ao.parent.apiObjects[jsonnetName]; ok {
		log.Panicf(
			"Tried to lowercase first character of object kind '%s', but lowercase name was already present in version '%s'",
			jsonnetName,
			ao.parent.version)
	}

	ao.comments.emit(m)

	m.writeLine(fmt.Sprintf("%s:: {", jsonnetName))
	m.indent()

	if ao.isTopLevel {
		// NOTE: It is important to NOT capitalize `ao.name` here.
		m.writeLine(fmt.Sprintf("local kind = {kind: \"%s\"},", ao.name))
	}
	ao.emitConstructors(m)

	for _, pm := range ao.properties.sortAndFilterBlacklisted() {
		// Skip special properties and fields that `$ref` another API
		// object type, since those will go in the `mixin` namespace.
		if isSpecialProperty(pm.name) || isMixinRef(pm.ref) {
			continue
		}
		pm.emit(m)
	}

	// Emit the properties that `$ref` another API object type in the
	// `mixin:: {` namespace.
	m.writeLine("mixin:: {")
	m.indent()

	for _, pm := range ao.properties.sortAndFilterBlacklisted() {
		// TODO: Emit mixin code also for arrays whose elements are
		// `$ref`.
		if !isMixinRef(pm.ref) {
			continue
		}

		pm.emit(m)
	}

	m.dedent()
	m.writeLine("},")

	m.dedent()
	m.writeLine("},")
}

// `emitAsRefMixins` recursively emits an API object as a collection
// of mixin methods, particularly when another API object has a
// property that uses `$ref` to reference the current API object.
//
// For example, `v1beta1.Deployment` has a field, `spec`, which is of
// type `v1beta1.DeploymentSpec`. In this case, we'd like to
// recursively capture all the properties of `v1beta1.DeploymentSpec`
// and create mixin methods, so that we can do something like
// `someDeployment + deployment.mixin.spec.minReadySeconds(3)`.
func (ao *apiObject) emitAsRefMixins(
	m *indentWriter, p *property, parentMixinName *string,
) {
	k8sVersion := ao.root().spec.Info.Version
	functionName := jsonnet.RewriteAsIdentifier(k8sVersion, p.name)
	paramName := jsonnet.RewriteAsFuncParam(k8sVersion, p.name)
	fieldName := jsonnet.RewriteAsFieldKey(p.name)
	mixinName := fmt.Sprintf("__%sMixin", functionName)
	var mixinText string
	if parentMixinName == nil {
		mixinText = fmt.Sprintf(
			"local %s(%s) = {%s+: %s},", mixinName, paramName, fieldName, paramName)
	} else {
		mixinText = fmt.Sprintf(
			"local %s(%s) = %s({%s+: %s}),",
			mixinName, paramName, *parentMixinName, fieldName, paramName)
	}

	if _, ok := ao.parent.apiObjects[kubespec.ObjectKind(functionName)]; ok {
		log.Panicf(
			"Tried to lowercase first character of object kind '%s', but lowercase name was already present in version '%s'",
			functionName,
			ao.parent.version)
	}

	// NOTE: Comments are emitted by `property#emit`, before we
	// call this method.

	line := fmt.Sprintf("%s:: {", functionName)
	m.writeLine(line)
	m.indent()

	m.writeLine(mixinText)
	m.writeLine(
		fmt.Sprintf("mixinInstance(%s):: %s(%s),", paramName, mixinName, paramName))

	for _, pm := range ao.properties.sortAndFilterBlacklisted() {
		if isSpecialProperty(pm.name) {
			continue
		}
		pm.emitAsRefMixin(m, mixinName)
	}

	m.dedent()
	m.writeLine("},")
}

func (ao *apiObject) emitConstructors(m *indentWriter) {
	k8sVersion := ao.root().spec.Info.Version
	path := ao.parsedName.Unparse()

	specs, ok := kubeversion.ConstructorSpec(k8sVersion, path)
	if !ok {
		ao.emitConstructor(m, constructorName, []kubeversion.CustomConstructorParam{})
		return
	}

	for _, spec := range specs {
		ao.emitConstructor(m, spec.ID, spec.Params)
	}
}

func (ao *apiObject) emitConstructor(
	m *indentWriter, id string, params []kubeversion.CustomConstructorParam,
) {
	// Panic if a function with the constructor's name already exists.
	specName := kubespec.PropertyName(id)
	if dm, ok := ao.properties[specName]; ok {
		log.Panicf(
			"Attempted to create constructor, but '%s' property already existed at '%s'",
			specName, dm.path)
	}

	// Default body of the constructor. Usually either `apiVersion +
	// kind` or `{}`.
	var defaultSetters []string
	if ao.isTopLevel {
		defaultSetters = specialPropertiesList
	} else {
		defaultSetters = []string{"{}"}
	}

	// Build parameters and body of constructor. Considering the example
	// of the constructor of `v1.Container`:
	//
	//   new(name, image):: self.name(name) + self.image(image),
	//
	// Here we want to (1) assemble the parameter list (i.e., `name` and
	// `image`), as well as the body (i.e., the calls to `self.name` and
	// so on).
	paramLiterals := []string{}
	setters := defaultSetters
	for _, param := range params {
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
			prop, ok := ao.properties[kubespec.PropertyName(param.ID)]
			if !ok {
				log.Panicf(
					"Attempted to create constructor, but property '%s' does not exist",
					param.ID)
			}
			k8sVersion := ao.root().spec.Info.Version
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
	m.writeLine(fmt.Sprintf("%s(%s):: %s,", specName, paramsText, bodyText))
}

func (aos apiObjectSet) toSortedSlice() apiObjectSlice {
	apiObjects := apiObjectSlice{}
	for _, apiObject := range aos {
		apiObjects = append(apiObjects, apiObject)
	}
	sort.Slice(apiObjects, func(i, j int) bool {
		return apiObjects[i].name < apiObjects[j].name
	})
	return apiObjects
}

//-----------------------------------------------------------------------------
// Ref mixin API Object
//-----------------------------------------------------------------------------

type refMixinAPIObject struct {
	apiObject
	prop            *property
	parentMixinName string
}

func (rmao *refMixinAPIObject) accept(visitor Visitor) {

	for _, pm := range rmao.properties.sortAndFilterBlacklisted() {
		if isSpecialProperty(pm.name) {
			continue
		}
		pm.parentMixinName = rmao.parentMixinName
		pm.accept(visitor)
	}

	visitor.visitRefMixinAPIObject(rmao)
}

//-----------------------------------------------------------------------------
// Property method.
//-----------------------------------------------------------------------------

type propertyKind int

const (
	method propertyKind = iota
	typeAlias
)

// `property` is an abstract representation of a ksonnet-lib's
// property methods, which can be emitted as Jsonnet code using the
// `emit` method.
//
// For example, ksonnet-lib exposes many functions such as
// `v1.container.image`, which can be added together with the `+`
// operator to construct a complete image. `property` is an
// abstract representation of these so-called "property methods".
//
// `property` contains the name of the property given in the
// `apiObject` that is its parent (for example, `Deployment` has a
// field called `containers`, which is an array of `v1.Container`), as
// well as the `kubespec.PropertyName`, which contains information
// required to generate the Jsonnet code.
//
// The logic for creating them is handled largely by `root`.
type property struct {
	kind       propertyKind
	ref        *kubespec.ObjectRef
	schemaType *kubespec.SchemaType
	itemTypes  kubespec.Items
	name       kubespec.PropertyName // e.g., image in container.image.
	path       kubespec.DefinitionName
	comments   comments
	parent     *apiObject

	parentMixinName string // visit context variable
}
type propertySet map[kubespec.PropertyName]*property
type propertySlice []*property

func newPropertyMethod(
	name kubespec.PropertyName, path kubespec.DefinitionName,
	prop *kubespec.Property, parent *apiObject,
) *property {
	comments := newComments(prop.Description)
	return &property{
		kind:       method,
		ref:        prop.Ref,
		schemaType: prop.Type,
		itemTypes:  prop.Items,
		name:       name,
		path:       path,
		comments:   comments,
		parent:     parent,
	}
}

func newPropertyTypeAlias(
	name kubespec.PropertyName, path kubespec.DefinitionName,
	prop *kubespec.Property, parent *apiObject,
) *property {
	comments := newComments(prop.Description)
	return &property{
		kind:       typeAlias,
		ref:        prop.Ref,
		schemaType: prop.Type,
		itemTypes:  prop.Items,
		name:       name,
		path:       path,
		comments:   comments,
		parent:     parent,
	}
}

func (p *property) root() *root {
	return p.parent.parent.parent.parent
}

func (p *property) accept(visitor Visitor) {

	if p.kind == typeAlias {
		visitor.visitTypeAlias(p)
		return
	}

	p.comments.accept(visitor)

	if isMixinRef(p.ref) {

		parsedRefPath := p.ref.Name().Parse()
		apiObject := p.root().getAPIObject(parsedRefPath)
		rmao := refMixinAPIObject{
			apiObject: *apiObject,
			prop:      p,
		}
		rmao.accept(visitor)
		return
	} else if p.ref != nil && !isMixinRef(p.ref) {
		visitor.visitRefProperty(p)
		return
	} else if p.schemaType == nil {
		log.Panicf("Neither a type nor a ref")
	}

	visitor.visitProperty(p)
}

func (p *property) emit(m *indentWriter) {
	p.emitHelper(m, nil)
}

// `emitAsRefMixin` will emit a property as a mixin method, so that it
// can be "mixed in" to alter an existing object.
//
// For example if we have a fully-formed deployment object,
// `someDeployment`, we'd like to be able to do something like
// `someDeployment + deployment.mixin.spec.minReadySeconds(3)` to "mix
// in" a change to the `spec.minReadySeconds` field.
//
// This method will take the `property`, which specifies a
// property method, and use it to emit such a "mixin method".
func (p *property) emitAsRefMixin(
	m *indentWriter, parentMixinName string,
) {
	p.emitHelper(m, &parentMixinName)
}

func (p *property) emitAsTypeAlias(m *indentWriter) {
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

	m.writeLine(line)
}

// `emitHelper` emits the Jsonnet program text for a `property`,
// handling both the case that it's a mixin (i.e., `parentMixinName !=
// nil`), and the case that it's a "normal", non-mixin property method
// (i.e., `parentMixinName == nil`).
//
// NOTE: To get `emitHelper` to emit this property as a mixin, it is
// REQUIRED for `parentMixinName` to be non-nil; likewise, to get
// `emitHelper` to emit this property as a normal, non-mixin property
// method, it is necessary for `parentMixinName == nil`.
func (p *property) emitHelper(
	m *indentWriter, parentMixinName *string,
) {
	if p.kind == typeAlias {
		p.emitAsTypeAlias(m)
		return
	}

	p.comments.emit(m)

	k8sVersion := p.root().spec.Info.Version
	setterFunctionName := jsonnet.RewriteAsIdentifier(k8sVersion, p.name).ToSetterID()
	mixinFunctionName := jsonnet.RewriteAsIdentifier(k8sVersion, p.name).ToMixinID()
	paramName := jsonnet.RewriteAsFuncParam(k8sVersion, p.name)
	fieldName := jsonnet.RewriteAsFieldKey(p.name)
	setterSignature := fmt.Sprintf("%s(%s)::", setterFunctionName, paramName)
	mixinSignature := fmt.Sprintf("%s(%s)::", mixinFunctionName, paramName)

	if isMixinRef(p.ref) {
		parsedRefPath := p.ref.Name().Parse()
		apiObject := p.root().getAPIObject(parsedRefPath)
		apiObject.emitAsRefMixins(m, p, parentMixinName)
	} else if p.ref != nil && !isMixinRef(p.ref) {
		var body string
		if parentMixinName == nil {
			body = fmt.Sprintf("{%s: %s}", fieldName, paramName)
		} else {
			body = fmt.Sprintf("%s({%s: %s})", *parentMixinName, fieldName, paramName)
		}
		line := fmt.Sprintf("%s %s,", setterSignature, body)
		m.writeLine(line)
	} else if p.schemaType != nil {
		paramType := *p.schemaType

		//
		// Generate both setter and mixin functions for some property. For
		// example, we emit both `metadata.setAnnotations({foo: "bar"})`
		// (which replaces a set of annotations with given object) and
		// `metadata.mixinAnnotations({foo: "bar"})` (which replaces only
		// the `foo` key, if it exists.)
		//

		var setterBody string
		var mixinBody string
		emitMixin := false
		switch paramType {
		case "array":
			emitMixin = true
			if parentMixinName == nil {
				setterBody = fmt.Sprintf(
					"if std.type(%s) == \"array\" then {%s: %s} else {%s: [%s]}",
					paramName, fieldName, paramName, fieldName, paramName,
				)
				mixinBody = fmt.Sprintf(
					"if std.type(%s) == \"array\" then {%s+: %s} else {%s+: [%s]}",
					paramName, fieldName, paramName, fieldName, paramName,
				)
			} else {
				setterBody = fmt.Sprintf(
					"if std.type(%s) == \"array\" then %s({%s: %s}) else %s({%s: [%s]})",
					paramName, *parentMixinName, fieldName, paramName, *parentMixinName,
					fieldName, paramName,
				)
				mixinBody = fmt.Sprintf(
					"if std.type(%s) == \"array\" then %s({%s+: %s}) else %s({%s+: [%s]})",
					paramName, *parentMixinName, fieldName, paramName, *parentMixinName,
					fieldName, paramName,
				)
			}
		case "integer", "string", "boolean":
			if parentMixinName == nil {
				setterBody = fmt.Sprintf("{%s: %s}", fieldName, paramName)
			} else {
				setterBody = fmt.Sprintf("%s({%s: %s})", *parentMixinName, fieldName, paramName)
			}
		case "object":
			emitMixin = true
			if parentMixinName == nil {
				setterBody = fmt.Sprintf("{%s: %s}", fieldName, paramName)
				mixinBody = fmt.Sprintf("{%s+: %s}", fieldName, paramName)
			} else {
				setterBody = fmt.Sprintf("%s({%s: %s})", *parentMixinName, fieldName, paramName)
				mixinBody = fmt.Sprintf("%s({%s+: %s})", *parentMixinName, fieldName, paramName)
			}
		default:
			log.Panicf("Unrecognized type '%s'", paramType)
		}

		//
		// Emit.
		//

		line := fmt.Sprintf("%s self + %s,", setterSignature, setterBody)
		m.writeLine(line)

		if emitMixin {
			p.comments.emit(m)
			line = fmt.Sprintf("%s self + %s,", mixinSignature, mixinBody)
			m.writeLine(line)
		}
	} else {
		log.Panicf("Neither a type nor a ref")
	}
}

func (aos propertySet) sortAndFilterBlacklisted() propertySlice {
	properties := propertySlice{}
	for _, pm := range aos {
		k8sVersion := pm.root().spec.Info.Version
		var name kubespec.PropertyName
		if pm.kind == typeAlias {
			name = kubespec.PropertyName(strings.TrimSuffix(string(pm.name), "Type"))
		} else {
			name = pm.name
		}
		if kubeversion.IsBlacklistedProperty(k8sVersion, pm.path, name) {
			continue
		} else if pm.ref != nil {
			if parsed := pm.ref.Name().Parse(); parsed.Version == nil {
				// TODO: Might want to error out here.
				continue
			}
		}
		properties = append(properties, pm)
	}
	sort.Slice(properties, func(i, j int) bool {
		return properties[i].name < properties[j].name
	})
	return properties
}

//-----------------------------------------------------------------------------
// Comments.
//-----------------------------------------------------------------------------

type comments []string

func newComments(text string) comments {
	return strings.Split(text, "\n")
}

func (cs *comments) accept(visitor Visitor) {
	visitor.visitComments(cs)
}

func (cs *comments) emit(m *indentWriter) {
	for _, comment := range *cs {
		if comment == "" {
			// Don't create trailing space if comment is empty.
			m.writeLine("//")
		} else {
			m.writeLine(fmt.Sprintf("// %s", comment))
		}
	}
}
