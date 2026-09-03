package conform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Errors returned when a model is not usable as a statement of intent.
var (
	// ErrNotBPMN is returned for a document that is not a BPMN 2.0 model.
	ErrNotBPMN = errors.New("conform: not a BPMN 2.0 model")
	// ErrBadModel is returned for a model that parses but does not describe a
	// process the engine can reason about.
	ErrBadModel = errors.New("conform: malformed process model")
	// ErrUnknownInvariant is returned when a model declares an invariant that
	// no implementation is registered for. It is a hard error, not a warning:
	// an intent with no evidence path is the exact failure this engine exists
	// to catch, and a model that declares a control nothing checks is worse
	// than a model that declares nothing, because it reads as assurance.
	ErrUnknownInvariant = errors.New("conform: model declares an invariant with no registered implementation")
	// ErrUnapprovedPath is returned when a money-movement task can be reached
	// from a start event without passing an approval gate.
	ErrUnapprovedPath = errors.New("conform: money-movement task is reachable without a human approval")
)

// bpmnNS is the BPMN 2.0 model namespace. A document in any other namespace is
// refused rather than parsed leniently: this package reads BPMN because BPMN is
// what a process owner draws, and accepting a lookalike would let the model and
// the drawing drift apart.
const bpmnNS = "http://www.omg.org/spec/BPMN/20100524/MODEL"

// ElementKind is the BPMN element type, restricted to the subset this parser
// understands. Anything outside the subset is rejected at parse time rather
// than ignored, so the model in the repository is always the model the engine
// read.
type ElementKind string

// The recognised element kinds.
const (
	KindStartEvent        ElementKind = "startEvent"
	KindEndEvent          ElementKind = "endEvent"
	KindTask              ElementKind = "task"
	KindUserTask          ElementKind = "userTask"
	KindServiceTask       ElementKind = "serviceTask"
	KindExclusiveGateway  ElementKind = "exclusiveGateway"
	KindParallelGateway   ElementKind = "parallelGateway"
	KindIntermediateEvent ElementKind = "intermediateCatchEvent"
)

// Directive is an annotation keyword. The syntax is small on purpose: a control
// annotation is a claim the engine has to be able to check, and a syntax with
// room for prose has room for a claim nothing checks.
type Directive string

// The recognised directives.
const (
	// DirectiveInvariant names an invariant id and states it in words.
	DirectiveInvariant Directive = "invariant"
	// DirectiveControl names the control the element implements.
	DirectiveControl Directive = "control"
	// DirectiveEvidence names a source and selector the control is evidenced by.
	DirectiveEvidence Directive = "evidence"
	// DirectiveTier names the risk tier of the work the element does.
	DirectiveTier Directive = "tier"
)

// Tier values a @tier directive may carry. They are the agent runtime's tiers,
// restated here so the parser does not depend on the runtime package.
const (
	TierRead  = "read_only"
	TierState = "state_change"
	TierMoney = "money_movement"
)

// Controls whose presence on a userTask makes it an approval gate. A userTask
// carrying any other control is a human step but not an approval: somebody
// looking at a screen is not the same thing as somebody accepting
// responsibility, and only the second one gates money movement.
const (
	ControlFourEyes      = "four-eyes"
	ControlDualApproval  = "dual-approval"
	ControlHumanApproval = "human-approval"
)

// evidenceSources are the sources an @evidence selector may name. An unknown
// source is a hard error because it means the model is pointing at evidence the
// engine has no way to collect.
var evidenceSources = map[string]bool{
	"audit":  true,
	"span":   true,
	"ledger": true,
	"bridge": true,
	"lake":   true,
	"policy": true,
}

// approvalControls is the set that turns a userTask into an approval gate.
var approvalControls = map[string]bool{
	ControlFourEyes:      true,
	ControlDualApproval:  true,
	ControlHumanApproval: true,
}

// Annotation is one parsed BPMN text annotation.
type Annotation struct {
	// ID is the textAnnotation element id.
	ID string `json:"id"`
	// AttachedTo lists the element ids the annotation is associated with, in
	// document order.
	AttachedTo []string `json:"attached_to,omitempty"`
	// Text is the annotation's full text, prose lines included.
	Text string `json:"text"`
	// Directives are the parsed @ lines, in the order they appear.
	Directives []DirectiveLine `json:"directives,omitempty"`
}

// DirectiveLine is one parsed @ line of an annotation.
type DirectiveLine struct {
	// Kind is the directive keyword.
	Kind Directive `json:"kind"`
	// Key is the first word after the keyword: an invariant id for
	// @invariant, a control name for @control, a tier for @tier, and the
	// source for @evidence.
	Key string `json:"key"`
	// Value is the remainder of the line: the invariant's statement in words
	// for @invariant, the selector for @evidence, empty otherwise.
	Value string `json:"value,omitempty"`
}

// Element is one node of the process graph.
type Element struct {
	// ID is the BPMN element id, which is what an association and a sequence
	// flow refer to and what an error message names.
	ID string `json:"id"`
	// Name is the label on the diagram.
	Name string `json:"name,omitempty"`
	// Kind is the BPMN element type.
	Kind ElementKind `json:"kind"`
	// Annotations are the annotations associated with this element, in
	// document order.
	Annotations []Annotation `json:"annotations,omitempty"`
}

// Tier returns the element's declared risk tier, or the empty string.
func (e Element) Tier() string {
	for _, a := range e.Annotations {
		for _, d := range a.Directives {
			if d.Kind == DirectiveTier {
				return d.Key
			}
		}
	}
	return ""
}

// Controls returns the controls the element declares, in document order.
func (e Element) Controls() []string {
	var out []string
	for _, a := range e.Annotations {
		for _, d := range a.Directives {
			if d.Kind == DirectiveControl {
				out = append(out, d.Key)
			}
		}
	}
	return out
}

// Invariants returns the invariant ids the element declares, in document order.
func (e Element) Invariants() []string {
	var out []string
	for _, a := range e.Annotations {
		for _, d := range a.Directives {
			if d.Kind == DirectiveInvariant {
				out = append(out, d.Key)
			}
		}
	}
	return out
}

// MovesMoney reports whether the element is declared money-movement.
func (e Element) MovesMoney() bool { return e.Tier() == TierMoney }

// IsApprovalGate reports whether the element is a userTask carrying an approval
// control.
func (e Element) IsApprovalGate() bool {
	if e.Kind != KindUserTask {
		return false
	}
	for _, c := range e.Controls() {
		if approvalControls[c] {
			return true
		}
	}
	return false
}

// Flow is one sequence flow of the process graph.
type Flow struct {
	// ID is the sequenceFlow id.
	ID string `json:"id"`
	// Name is the branch label, such as "allow" or "no quorum".
	Name string `json:"name,omitempty"`
	// SourceRef and TargetRef are the element ids the flow joins. They are the
	// single source of truth for the graph: the incoming and outgoing child
	// elements BPMN also permits are ignored.
	SourceRef string `json:"source_ref"`
	TargetRef string `json:"target_ref"`
}

// ControlPoint is an annotated element, positioned in the process. The order is
// the order a reader of the diagram would walk it, which is the order a report
// has to present so that a verdict can be read against the drawing.
type ControlPoint struct {
	// Position is the element's zero-based place in the traversal.
	Position int `json:"position"`
	// Element is the annotated element.
	Element Element `json:"element"`
	// Controls are the controls declared at this point.
	Controls []string `json:"controls,omitempty"`
	// Invariants are the invariant ids declared at this point.
	Invariants []string `json:"invariants,omitempty"`
}

// Process is a parsed BPMN process: the graph, its annotations, and the digest
// of the document it came from.
//
// The digest is part of the value because a verdict is a statement about a
// specific intent. "The control held" is only meaningful alongside "and this is
// the exact model text that declared it", and a bundle that carried the
// verdicts without the model digest would let the intent be edited afterwards.
type Process struct {
	// ID is the process id an evidence bundle names.
	ID string `json:"id"`
	// Name is the label on the diagram.
	Name string `json:"name,omitempty"`
	// Executable mirrors isExecutable. Nothing here executes it; the flag is
	// carried so a model marked as documentation-only is visible as such.
	Executable bool `json:"executable"`
	// Elements are the flow nodes, grouped by kind and in document order
	// within a kind.
	Elements []Element `json:"elements"`
	// Flows are the sequence flows in document order.
	Flows []Flow `json:"flows"`
	// Annotations are the parsed text annotations in document order.
	Annotations []Annotation `json:"annotations,omitempty"`
	// SourceDigest is the sha256 of the document this process was parsed from.
	SourceDigest string `json:"source_digest"`

	index map[string]int
	out   map[string][]Flow
}

// Lookup returns an element by id.
func (p *Process) Lookup(id string) (Element, bool) {
	i, ok := p.index[id]
	if !ok {
		return Element{}, false
	}
	return p.Elements[i], true
}

// StartEvents returns the process's start events in document order.
func (p *Process) StartEvents() []Element {
	var out []Element
	for _, e := range p.Elements {
		if e.Kind == KindStartEvent {
			out = append(out, e)
		}
	}
	return out
}

// DeclaredInvariants returns the invariant ids the model declares, sorted and
// deduplicated. This is the set the engine evaluates: the model chooses what is
// checked, not the code.
func (p *Process) DeclaredInvariants() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range p.Annotations {
		for _, d := range a.Directives {
			if d.Kind == DirectiveInvariant && !seen[d.Key] {
				seen[d.Key] = true
				out = append(out, d.Key)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ControlPoints returns the annotated elements in traversal order. Elements
// unreachable from a start event come last, in document order, so that a broken
// model still renders rather than silently dropping the part that is wrong.
func (p *Process) ControlPoints() []ControlPoint {
	order := p.traversal()
	rank := make(map[string]int, len(order))
	for i, id := range order {
		rank[id] = i
	}
	type scored struct {
		rank int
		doc  int
		el   Element
	}
	var annotated []scored
	for i, e := range p.Elements {
		if len(e.Annotations) == 0 {
			continue
		}
		r, ok := rank[e.ID]
		if !ok {
			r = len(order) + i
		}
		annotated = append(annotated, scored{rank: r, doc: i, el: e})
	}
	sort.SliceStable(annotated, func(i, j int) bool {
		if annotated[i].rank != annotated[j].rank {
			return annotated[i].rank < annotated[j].rank
		}
		return annotated[i].doc < annotated[j].doc
	})
	out := make([]ControlPoint, 0, len(annotated))
	for i, s := range annotated {
		out = append(out, ControlPoint{
			Position:   i,
			Element:    s.el,
			Controls:   s.el.Controls(),
			Invariants: s.el.Invariants(),
		})
	}
	return out
}

// traversal returns element ids in breadth-first order from the start events,
// following sequence flows in document order. It is deterministic, which is
// what lets a report be diffed between runs.
func (p *Process) traversal() []string {
	var queue []string
	seen := map[string]bool{}
	for _, e := range p.StartEvents() {
		if !seen[e.ID] {
			seen[e.ID] = true
			queue = append(queue, e.ID)
		}
	}
	var out []string
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		out = append(out, id)
		for _, f := range p.out[id] {
			if !seen[f.TargetRef] {
				seen[f.TargetRef] = true
				queue = append(queue, f.TargetRef)
			}
		}
	}
	return out
}

// ParseFile reads and parses a BPMN model from disk.
func ParseFile(path string) (*Process, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// ---------- XML shapes ----------

type xmlDefinitions struct {
	XMLName   xml.Name     `xml:"definitions"`
	Processes []xmlProcess `xml:"process"`
}

type xmlProcess struct {
	ID           string          `xml:"id,attr"`
	Name         string          `xml:"name,attr"`
	IsExecutable string          `xml:"isExecutable,attr"`
	Start        []xmlNode       `xml:"startEvent"`
	End          []xmlNode       `xml:"endEvent"`
	Task         []xmlNode       `xml:"task"`
	UserTask     []xmlNode       `xml:"userTask"`
	ServiceTask  []xmlNode       `xml:"serviceTask"`
	Exclusive    []xmlNode       `xml:"exclusiveGateway"`
	Parallel     []xmlNode       `xml:"parallelGateway"`
	Intermediate []xmlNode       `xml:"intermediateCatchEvent"`
	Flows        []xmlFlow       `xml:"sequenceFlow"`
	Notes        []xmlAnnotation `xml:"textAnnotation"`
	Assocs       []xmlAssoc      `xml:"association"`
}

type xmlNode struct {
	ID   string `xml:"id,attr"`
	Name string `xml:"name,attr"`
}

type xmlFlow struct {
	ID     string `xml:"id,attr"`
	Name   string `xml:"name,attr"`
	Source string `xml:"sourceRef,attr"`
	Target string `xml:"targetRef,attr"`
}

type xmlAnnotation struct {
	ID   string `xml:"id,attr"`
	Text string `xml:"text"`
}

type xmlAssoc struct {
	ID     string `xml:"id,attr"`
	Source string `xml:"sourceRef,attr"`
	Target string `xml:"targetRef,attr"`
}

// Parse reads a BPMN 2.0 document and returns its single process.
//
// A document carrying more than one process is refused. Nothing in this
// platform needs two, and a parser that silently picked the first would make
// which process was checked depend on element order.
func Parse(data []byte) (*Process, error) {
	var defs xmlDefinitions
	if err := xml.Unmarshal(data, &defs); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotBPMN, err)
	}
	if defs.XMLName.Space != bpmnNS {
		return nil, fmt.Errorf("%w: root element is in namespace %q, want %q",
			ErrNotBPMN, defs.XMLName.Space, bpmnNS)
	}
	switch len(defs.Processes) {
	case 1:
	case 0:
		return nil, fmt.Errorf("%w: document declares no process", ErrBadModel)
	default:
		return nil, fmt.Errorf("%w: document declares %d processes, want exactly one",
			ErrBadModel, len(defs.Processes))
	}
	xp := defs.Processes[0]
	if xp.ID == "" {
		return nil, fmt.Errorf("%w: process has no id", ErrBadModel)
	}

	sum := sha256.Sum256(data)
	p := &Process{
		ID:           xp.ID,
		Name:         xp.Name,
		Executable:   xp.IsExecutable == "true",
		SourceDigest: hex.EncodeToString(sum[:]),
		index:        map[string]int{},
		out:          map[string][]Flow{},
	}

	// Elements, grouped by kind but kept in a stable order so that a report is
	// diffable: kind order first, document order within a kind.
	groups := []struct {
		kind  ElementKind
		nodes []xmlNode
	}{
		{KindStartEvent, xp.Start},
		{KindUserTask, xp.UserTask},
		{KindServiceTask, xp.ServiceTask},
		{KindTask, xp.Task},
		{KindExclusiveGateway, xp.Exclusive},
		{KindParallelGateway, xp.Parallel},
		{KindIntermediateEvent, xp.Intermediate},
		{KindEndEvent, xp.End},
	}
	for _, g := range groups {
		for _, n := range g.nodes {
			if n.ID == "" {
				return nil, fmt.Errorf("%w: a %s has no id", ErrBadModel, g.kind)
			}
			if _, dup := p.index[n.ID]; dup {
				return nil, fmt.Errorf("%w: duplicate element id %q", ErrBadModel, n.ID)
			}
			p.index[n.ID] = len(p.Elements)
			p.Elements = append(p.Elements, Element{ID: n.ID, Name: n.Name, Kind: g.kind})
		}
	}
	if len(p.Elements) == 0 {
		return nil, fmt.Errorf("%w: process %q has no elements this parser understands", ErrBadModel, p.ID)
	}

	// Annotations.
	notes := make(map[string]int, len(xp.Notes))
	for _, n := range xp.Notes {
		if n.ID == "" {
			return nil, fmt.Errorf("%w: a textAnnotation has no id", ErrBadModel)
		}
		if _, dup := notes[n.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate textAnnotation id %q", ErrBadModel, n.ID)
		}
		a, err := parseAnnotation(n.ID, n.Text)
		if err != nil {
			return nil, err
		}
		notes[n.ID] = len(p.Annotations)
		p.Annotations = append(p.Annotations, a)
	}

	// Associations bind annotations to elements. BPMN tools disagree about
	// which end of the association is the annotation, so whichever end names a
	// textAnnotation is taken as the annotation and the other as the element.
	for _, as := range xp.Assocs {
		noteID, elemID := as.Target, as.Source
		if _, ok := notes[as.Source]; ok {
			noteID, elemID = as.Source, as.Target
		}
		ni, ok := notes[noteID]
		if !ok {
			return nil, fmt.Errorf("%w: association %q joins no textAnnotation", ErrBadModel, as.ID)
		}
		ei, ok := p.index[elemID]
		if !ok {
			return nil, fmt.Errorf("%w: association %q attaches annotation %q to unknown element %q",
				ErrBadModel, as.ID, noteID, elemID)
		}
		p.Annotations[ni].AttachedTo = append(p.Annotations[ni].AttachedTo, elemID)
		p.Elements[ei].Annotations = append(p.Elements[ei].Annotations, p.Annotations[ni])
	}

	// Flows.
	seenFlow := map[string]bool{}
	for _, f := range xp.Flows {
		if f.ID == "" {
			return nil, fmt.Errorf("%w: a sequenceFlow has no id", ErrBadModel)
		}
		if seenFlow[f.ID] {
			return nil, fmt.Errorf("%w: duplicate sequenceFlow id %q", ErrBadModel, f.ID)
		}
		seenFlow[f.ID] = true
		if _, ok := p.index[f.Source]; !ok {
			return nil, fmt.Errorf("%w: sequenceFlow %q leaves unknown element %q", ErrBadModel, f.ID, f.Source)
		}
		if _, ok := p.index[f.Target]; !ok {
			return nil, fmt.Errorf("%w: sequenceFlow %q enters unknown element %q", ErrBadModel, f.ID, f.Target)
		}
		fl := Flow{ID: f.ID, Name: f.Name, SourceRef: f.Source, TargetRef: f.Target}
		p.Flows = append(p.Flows, fl)
		p.out[f.Source] = append(p.out[f.Source], fl)
	}
	return p, nil
}

// parseAnnotation reads the small directive syntax out of an annotation body.
func parseAnnotation(id, text string) (Annotation, error) {
	a := Annotation{ID: id, Text: strings.TrimSpace(text)}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || !strings.HasPrefix(line, "@") {
			continue // prose, kept in Text and otherwise ignored
		}
		word, rest, _ := strings.Cut(strings.TrimPrefix(line, "@"), " ")
		rest = strings.TrimSpace(rest)
		d := DirectiveLine{Kind: Directive(word)}
		switch d.Kind {
		case DirectiveInvariant:
			d.Key, d.Value, _ = strings.Cut(rest, " ")
			d.Value = strings.TrimSpace(d.Value)
			if d.Key == "" {
				return a, fmt.Errorf("%w: annotation %q declares @invariant with no id", ErrBadModel, id)
			}
			if d.Value == "" {
				return a, fmt.Errorf("%w: annotation %q declares @invariant %s with no statement",
					ErrBadModel, id, d.Key)
			}
		case DirectiveControl:
			d.Key = rest
			if d.Key == "" {
				return a, fmt.Errorf("%w: annotation %q declares @control with no name", ErrBadModel, id)
			}
		case DirectiveTier:
			d.Key = rest
			switch d.Key {
			case TierRead, TierState, TierMoney:
			default:
				return a, fmt.Errorf("%w: annotation %q declares unknown @tier %q", ErrBadModel, id, d.Key)
			}
		case DirectiveEvidence:
			src, sel, ok := strings.Cut(rest, ":")
			if !ok || sel == "" {
				return a, fmt.Errorf("%w: annotation %q declares @evidence %q, want source:selector",
					ErrBadModel, id, rest)
			}
			if !evidenceSources[src] {
				return a, fmt.Errorf("%w: annotation %q names evidence source %q, which nothing collects",
					ErrBadModel, id, src)
			}
			d.Key, d.Value = src, sel
		default:
			return a, fmt.Errorf("%w: annotation %q uses unknown directive @%s", ErrBadModel, id, word)
		}
		a.Directives = append(a.Directives, d)
	}
	return a, nil
}

// Validate runs the structural checks that can be made against the model alone,
// plus the check that every invariant it declares is implemented.
//
// The checks are ordered from the local to the global so the first error a
// modeller sees is the most specific one available.
func (p *Process) Validate(reg *Registry) error {
	if len(p.StartEvents()) == 0 {
		return fmt.Errorf("%w: process %q has no start event", ErrBadModel, p.ID)
	}
	ends := 0
	for _, e := range p.Elements {
		if e.Kind == KindEndEvent {
			ends++
		}
	}
	if ends == 0 {
		return fmt.Errorf("%w: process %q has no end event", ErrBadModel, p.ID)
	}

	// Every element has to be reachable. An annotated element nothing can
	// reach is a control that never runs, and it is worth failing the build
	// over precisely because it looks fine on the diagram.
	reached := map[string]bool{}
	for _, id := range p.traversal() {
		reached[id] = true
	}
	for _, e := range p.Elements {
		if !reached[e.ID] {
			return fmt.Errorf("%w: element %q (%s) is unreachable from any start event",
				ErrBadModel, e.ID, e.Kind)
		}
	}

	if err := p.checkApprovalDominance(); err != nil {
		return err
	}

	if reg == nil {
		return nil
	}
	for _, id := range p.DeclaredInvariants() {
		if _, ok := reg.Get(id); !ok {
			return fmt.Errorf("%w: %q declared by process %q", ErrUnknownInvariant, id, p.ID)
		}
	}
	return nil
}

// checkApprovalDominance proves that every money-movement element is dominated
// by an approval gate.
//
// The test is not a search for an approval somewhere upstream, which a single
// unguarded branch would satisfy. It is the standard dominance test, run
// backwards: delete every approval gate from the graph and ask what is still
// reachable from a start event. Anything a walk can still arrive at has a path
// that never passed through an approval, and the walk itself is the witness —
// so the error names the path rather than only the task, because "this task is
// unguarded" is an assertion and "here is the sequence of flows that gets to it
// without an approval" is evidence.
//
// Cycles are handled by the visited set: a loop cannot create a path that
// avoids a gate the straight-line path went through.
func (p *Process) checkApprovalDominance() error {
	var money []Element
	for _, e := range p.Elements {
		if e.MovesMoney() {
			money = append(money, e)
		}
	}
	if len(money) == 0 {
		return nil
	}

	// Breadth-first from every start event, refusing to enter an approval gate.
	from := map[string]string{}
	seen := map[string]bool{}
	var queue []string
	for _, s := range p.StartEvents() {
		if s.IsApprovalGate() {
			continue
		}
		seen[s.ID] = true
		queue = append(queue, s.ID)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, f := range p.out[id] {
			if seen[f.TargetRef] {
				continue
			}
			next, ok := p.Lookup(f.TargetRef)
			if !ok {
				continue
			}
			seen[f.TargetRef] = true
			from[f.TargetRef] = id
			if next.IsApprovalGate() {
				// The gate is reached but not entered: this is what makes the
				// walk a dominance test rather than plain reachability.
				continue
			}
			queue = append(queue, f.TargetRef)
		}
	}

	for _, m := range money {
		if !seen[m.ID] || m.IsApprovalGate() {
			continue
		}
		return fmt.Errorf("%w: %q (%s) via %s",
			ErrUnapprovedPath, m.ID, m.Name, renderPath(pathTo(from, m.ID)))
	}
	return nil
}

// pathTo reconstructs the witness path from a start event to id.
func pathTo(from map[string]string, id string) []string {
	path := []string{id}
	for {
		prev, ok := from[id]
		if !ok {
			break
		}
		path = append([]string{prev}, path...)
		id = prev
	}
	return path
}

func renderPath(ids []string) string { return strings.Join(ids, " -> ") }
