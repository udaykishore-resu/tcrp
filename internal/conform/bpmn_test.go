package conform_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/conform"
)

// modelsDir is the checked-in BPMN directory, three levels up from this package.
func modelsDir() string { return filepath.Join("..", "..", "bpmn") }

func TestParseShippedModels(t *testing.T) {
	reg := conform.Default()
	cases := []struct {
		file       string
		process    string
		invariants []string
	}{
		{
			file:    "collateral-margin-call.bpmn",
			process: "tcrp-collateral-margin-call",
			invariants: []string{
				"AGENT-1", "AGENT-2", "AGENT-3", "AGENT-4", "AGENT-5", "AGENT-6", "AGENT-7",
			},
		},
		{
			file:    "bridge-lock-and-mint.bpmn",
			process: "tcrp-bridge-lock-and-mint",
			invariants: []string{
				"AGENT-1", "DATA-1", "SETTLE-1", "SETTLE-2", "SETTLE-3", "SETTLE-4", "SETTLE-5",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			p, err := conform.ParseFile(filepath.Join(modelsDir(), tc.file))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if p.ID != tc.process {
				t.Fatalf("process id %q, want %q", p.ID, tc.process)
			}
			if !p.Executable {
				t.Fatal("process is not marked executable")
			}
			if p.SourceDigest == "" {
				t.Fatal("process carries no source digest")
			}
			if err := p.Validate(reg); err != nil {
				t.Fatalf("validate: %v", err)
			}
			got := p.DeclaredInvariants()
			if strings.Join(got, ",") != strings.Join(tc.invariants, ",") {
				t.Fatalf("declared invariants %v, want %v", got, tc.invariants)
			}
			if len(p.ControlPoints()) == 0 {
				t.Fatal("no control points derived")
			}
		})
	}
}

// TestControlPointsFollowTheDiagram asserts the derived order is the order a
// reader walks the drawing, not the order the XML happens to list elements in.
func TestControlPointsFollowTheDiagram(t *testing.T) {
	p, err := conform.ParseFile(filepath.Join(modelsDir(), "collateral-margin-call.bpmn"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var ids []string
	for i, cp := range p.ControlPoints() {
		if cp.Position != i {
			t.Fatalf("control point %d reports position %d", i, cp.Position)
		}
		ids = append(ids, cp.Element.ID)
	}
	want := []string{
		"Start_Breach", "Task_QueryExposure", "Task_Guard", "Task_Propose",
		"Task_PolicyGate", "Task_ApproveCall", "End_Refused",
		"Task_IssueCall", "Task_SettleCollateral", "Task_SealAudit",
	}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("control point order\n got %v\nwant %v", ids, want)
	}
}

func TestParseRejectsMalformedModels(t *testing.T) {
	cases := []struct {
		name string
		xml  string
		want error
	}{
		{
			name: "not xml at all",
			xml:  "this is not a process model",
			want: conform.ErrNotBPMN,
		},
		{
			name: "wrong namespace",
			xml: `<definitions xmlns="http://example.com/notbpmn">
				<process id="p"/></definitions>`,
			want: conform.ErrNotBPMN,
		},
		{
			name: "no process",
			xml:  wrap(``),
			want: conform.ErrBadModel,
		},
		{
			name: "two processes",
			xml: `<bpmn:definitions xmlns:bpmn="http://www.omg.org/spec/BPMN/20100524/MODEL">
				<bpmn:process id="a"/><bpmn:process id="b"/></bpmn:definitions>`,
			want: conform.ErrBadModel,
		},
		{
			name: "flow into an element that does not exist",
			xml: wrap(`<bpmn:process id="p">
				<bpmn:startEvent id="s"/>
				<bpmn:endEvent id="e"/>
				<bpmn:sequenceFlow id="f" sourceRef="s" targetRef="ghost"/>
			</bpmn:process>`),
			want: conform.ErrBadModel,
		},
		{
			name: "duplicate element id",
			xml: wrap(`<bpmn:process id="p">
				<bpmn:startEvent id="s"/>
				<bpmn:endEvent id="s"/>
			</bpmn:process>`),
			want: conform.ErrBadModel,
		},
		{
			name: "unknown directive",
			xml: wrap(`<bpmn:process id="p">
				<bpmn:startEvent id="s"/>
				<bpmn:textAnnotation id="n"><bpmn:text>@assurance strong</bpmn:text></bpmn:textAnnotation>
				<bpmn:association id="a" sourceRef="s" targetRef="n"/>
			</bpmn:process>`),
			want: conform.ErrBadModel,
		},
		{
			name: "evidence source nothing collects",
			xml: wrap(`<bpmn:process id="p">
				<bpmn:startEvent id="s"/>
				<bpmn:textAnnotation id="n"><bpmn:text>@evidence vibes:strong</bpmn:text></bpmn:textAnnotation>
				<bpmn:association id="a" sourceRef="s" targetRef="n"/>
			</bpmn:process>`),
			want: conform.ErrBadModel,
		},
		{
			name: "invariant with no statement",
			xml: wrap(`<bpmn:process id="p">
				<bpmn:startEvent id="s"/>
				<bpmn:textAnnotation id="n"><bpmn:text>@invariant AGENT-1</bpmn:text></bpmn:textAnnotation>
				<bpmn:association id="a" sourceRef="s" targetRef="n"/>
			</bpmn:process>`),
			want: conform.ErrBadModel,
		},
		{
			name: "annotation attached to nothing",
			xml: wrap(`<bpmn:process id="p">
				<bpmn:startEvent id="s"/>
				<bpmn:textAnnotation id="n"><bpmn:text>@control four-eyes</bpmn:text></bpmn:textAnnotation>
				<bpmn:association id="a" sourceRef="ghost" targetRef="n"/>
			</bpmn:process>`),
			want: conform.ErrBadModel,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := conform.Parse([]byte(tc.xml))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v, want %v", err, tc.want)
			}
		})
	}
}

func TestValidateRejectsAnInvariantNothingImplements(t *testing.T) {
	src := wrap(`<bpmn:process id="p" name="p">
		<bpmn:startEvent id="s"/>
		<bpmn:endEvent id="e"/>
		<bpmn:sequenceFlow id="f" sourceRef="s" targetRef="e"/>
		<bpmn:textAnnotation id="n"><bpmn:text>@invariant AGENT-99 nobody wrote this one</bpmn:text></bpmn:textAnnotation>
		<bpmn:association id="a" sourceRef="s" targetRef="n"/>
	</bpmn:process>`)
	p, err := conform.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	err = p.Validate(conform.Default())
	if !errors.Is(err, conform.ErrUnknownInvariant) {
		t.Fatalf("error %v, want ErrUnknownInvariant", err)
	}
	if !strings.Contains(err.Error(), "AGENT-99") {
		t.Fatalf("error does not name the invariant: %v", err)
	}
}

func TestValidateRejectsAnUnreachableElement(t *testing.T) {
	src := wrap(`<bpmn:process id="p">
		<bpmn:startEvent id="s"/>
		<bpmn:endEvent id="e"/>
		<bpmn:serviceTask id="orphan" name="never runs"/>
		<bpmn:sequenceFlow id="f" sourceRef="s" targetRef="e"/>
	</bpmn:process>`)
	p, err := conform.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := p.Validate(nil); !errors.Is(err, conform.ErrBadModel) {
		t.Fatalf("error %v, want ErrBadModel", err)
	}
}

// TestReachabilityRejectsAnUnapprovedPathToMoney is the structural check the
// whole BPMN layer exists for: the model below has an approval on one branch
// and a straight line to the money task on the other, which a string match for
// "is there a userTask upstream" would accept.
func TestReachabilityRejectsAnUnapprovedPathToMoney(t *testing.T) {
	src := wrap(`<bpmn:process id="p" name="bypass">
		<bpmn:startEvent id="s"/>
		<bpmn:exclusiveGateway id="gw" name="urgent?"/>
		<bpmn:userTask id="approve" name="approve"/>
		<bpmn:serviceTask id="pay" name="settle the transfer"/>
		<bpmn:endEvent id="e"/>
		<bpmn:sequenceFlow id="f1" sourceRef="s" targetRef="gw"/>
		<bpmn:sequenceFlow id="f2" name="no"  sourceRef="gw" targetRef="approve"/>
		<bpmn:sequenceFlow id="f3" name="yes" sourceRef="gw" targetRef="pay"/>
		<bpmn:sequenceFlow id="f4" sourceRef="approve" targetRef="pay"/>
		<bpmn:sequenceFlow id="f5" sourceRef="pay" targetRef="e"/>
		<bpmn:textAnnotation id="n1"><bpmn:text>@control four-eyes</bpmn:text></bpmn:textAnnotation>
		<bpmn:association id="a1" sourceRef="approve" targetRef="n1"/>
		<bpmn:textAnnotation id="n2"><bpmn:text>@tier money_movement</bpmn:text></bpmn:textAnnotation>
		<bpmn:association id="a2" sourceRef="pay" targetRef="n2"/>
	</bpmn:process>`)
	p, err := conform.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	err = p.Validate(conform.Default())
	if !errors.Is(err, conform.ErrUnapprovedPath) {
		t.Fatalf("error %v, want ErrUnapprovedPath", err)
	}
	// The error has to name the witness path, not just the task: "this is
	// unguarded" is an assertion, the path is the evidence.
	for _, want := range []string{"pay", "s -> gw -> pay"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

// TestReachabilityAcceptsAnApprovalOnEveryPath is the same graph with the
// bypass removed.
func TestReachabilityAcceptsAnApprovalOnEveryPath(t *testing.T) {
	src := wrap(`<bpmn:process id="p" name="guarded">
		<bpmn:startEvent id="s"/>
		<bpmn:exclusiveGateway id="gw" name="urgent?"/>
		<bpmn:userTask id="approve" name="approve"/>
		<bpmn:userTask id="approve_fast" name="approve, expedited"/>
		<bpmn:serviceTask id="pay" name="settle the transfer"/>
		<bpmn:endEvent id="e"/>
		<bpmn:sequenceFlow id="f1" sourceRef="s" targetRef="gw"/>
		<bpmn:sequenceFlow id="f2" sourceRef="gw" targetRef="approve"/>
		<bpmn:sequenceFlow id="f3" sourceRef="gw" targetRef="approve_fast"/>
		<bpmn:sequenceFlow id="f4" sourceRef="approve" targetRef="pay"/>
		<bpmn:sequenceFlow id="f5" sourceRef="approve_fast" targetRef="pay"/>
		<bpmn:sequenceFlow id="f6" sourceRef="pay" targetRef="e"/>
		<bpmn:textAnnotation id="n1"><bpmn:text>@control four-eyes</bpmn:text></bpmn:textAnnotation>
		<bpmn:association id="a1" sourceRef="approve" targetRef="n1"/>
		<bpmn:textAnnotation id="n3"><bpmn:text>@control dual-approval</bpmn:text></bpmn:textAnnotation>
		<bpmn:association id="a3" sourceRef="approve_fast" targetRef="n3"/>
		<bpmn:textAnnotation id="n2"><bpmn:text>@tier money_movement</bpmn:text></bpmn:textAnnotation>
		<bpmn:association id="a2" sourceRef="pay" targetRef="n2"/>
	</bpmn:process>`)
	p, err := conform.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := p.Validate(conform.Default()); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestReachabilityIsNotFooledByALoopBackPastTheGate checks the case a plain
// "is an approval upstream" walk gets wrong: a retry edge that re-enters the
// money task from after it.
func TestReachabilityIsNotFooledByALoopBackPastTheGate(t *testing.T) {
	src := wrap(`<bpmn:process id="p" name="loop">
		<bpmn:startEvent id="s"/>
		<bpmn:userTask id="approve" name="approve"/>
		<bpmn:serviceTask id="pay" name="settle"/>
		<bpmn:exclusiveGateway id="gw" name="retry?"/>
		<bpmn:endEvent id="e"/>
		<bpmn:sequenceFlow id="f1" sourceRef="s" targetRef="approve"/>
		<bpmn:sequenceFlow id="f2" sourceRef="approve" targetRef="pay"/>
		<bpmn:sequenceFlow id="f3" sourceRef="pay" targetRef="gw"/>
		<bpmn:sequenceFlow id="f4" name="retry" sourceRef="gw" targetRef="pay"/>
		<bpmn:sequenceFlow id="f5" name="done" sourceRef="gw" targetRef="e"/>
		<bpmn:textAnnotation id="n1"><bpmn:text>@control four-eyes</bpmn:text></bpmn:textAnnotation>
		<bpmn:association id="a1" sourceRef="approve" targetRef="n1"/>
		<bpmn:textAnnotation id="n2"><bpmn:text>@tier money_movement</bpmn:text></bpmn:textAnnotation>
		<bpmn:association id="a2" sourceRef="pay" targetRef="n2"/>
	</bpmn:process>`)
	p, err := conform.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := p.Validate(conform.Default()); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestAUserTaskWithoutAnApprovalControlIsNotAGate: a human step is not the same
// thing as a human accepting responsibility, and a mis-typed control name must
// make the check stricter rather than weaker.
func TestAUserTaskWithoutAnApprovalControlIsNotAGate(t *testing.T) {
	src := wrap(`<bpmn:process id="p" name="review only">
		<bpmn:startEvent id="s"/>
		<bpmn:userTask id="review" name="have a look"/>
		<bpmn:serviceTask id="pay" name="settle"/>
		<bpmn:endEvent id="e"/>
		<bpmn:sequenceFlow id="f1" sourceRef="s" targetRef="review"/>
		<bpmn:sequenceFlow id="f2" sourceRef="review" targetRef="pay"/>
		<bpmn:sequenceFlow id="f3" sourceRef="pay" targetRef="e"/>
		<bpmn:textAnnotation id="n1"><bpmn:text>@control four_eyes</bpmn:text></bpmn:textAnnotation>
		<bpmn:association id="a1" sourceRef="review" targetRef="n1"/>
		<bpmn:textAnnotation id="n2"><bpmn:text>@tier money_movement</bpmn:text></bpmn:textAnnotation>
		<bpmn:association id="a2" sourceRef="pay" targetRef="n2"/>
	</bpmn:process>`)
	p, err := conform.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := p.Validate(conform.Default()); !errors.Is(err, conform.ErrUnapprovedPath) {
		t.Fatalf("error %v, want ErrUnapprovedPath", err)
	}
}

// TestAssociationDirectionDoesNotMatter: BPMN tools disagree about which end of
// an association names the annotation, and the parser accepts either.
func TestAssociationDirectionDoesNotMatter(t *testing.T) {
	for _, assoc := range []string{
		`<bpmn:association id="a" sourceRef="s" targetRef="n"/>`,
		`<bpmn:association id="a" sourceRef="n" targetRef="s"/>`,
	} {
		src := wrap(`<bpmn:process id="p">
			<bpmn:startEvent id="s"/>
			<bpmn:endEvent id="e"/>
			<bpmn:sequenceFlow id="f" sourceRef="s" targetRef="e"/>
			<bpmn:textAnnotation id="n"><bpmn:text>@control guardrail</bpmn:text></bpmn:textAnnotation>
			` + assoc + `
		</bpmn:process>`)
		p, err := conform.Parse([]byte(src))
		if err != nil {
			t.Fatalf("parse %s: %v", assoc, err)
		}
		el, ok := p.Lookup("s")
		if !ok || len(el.Controls()) != 1 || el.Controls()[0] != "guardrail" {
			t.Fatalf("annotation did not attach with %s", assoc)
		}
	}
}

// wrap puts a fragment inside a BPMN definitions element.
func wrap(inner string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<bpmn:definitions xmlns:bpmn="http://www.omg.org/spec/BPMN/20100524/MODEL">
` + inner + `
</bpmn:definitions>`
}
