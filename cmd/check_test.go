package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/guettli/konmon/internal/checks"
)

func TestRepeatCheckWhile(t *testing.T) {
	var msg bytes.Buffer
	runs := 0
	err := repeatCheck(context.Background(), &msg, time.Millisecond, true, func(context.Context) (bool, error) {
		runs++
		switch runs {
		case 1:
			return true, errFindings // still matching
		case 2:
			return false, errors.New("list pods: unexpected error") // retried after a successful run
		case 3:
			return false, errIncomplete // nothing matches, but not everything was listed
		case 4:
			return true, nil
		}
		return false, errFindings // findings, but none matches
	})
	if err != nil || runs != 5 {
		t.Fatalf("err %v after %d runs", err, runs)
	}
	for _, want := range []string{"Trying again", "Not everything could be listed"} {
		if !strings.Contains(msg.String(), want) {
			t.Errorf("missing %q in:\n%s", want, msg.String())
		}
	}
}

func TestRepeatCheckStopsOnAnErrorBeforeTheFirstRun(t *testing.T) {
	runs := 0
	err := repeatCheck(context.Background(), &bytes.Buffer{}, time.Millisecond, true, func(context.Context) (bool, error) {
		runs++
		return false, errors.New("--check-param: no check x")
	})
	if err == nil || runs != 1 {
		t.Errorf("err %v after %d runs", err, runs)
	}
}

func TestRetryCheck(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	bad := errors.New("bad flag")
	for _, c := range []struct {
		name     string
		retries  int
		errs     []error
		wantRuns int
		wantErr  error
	}{
		{"network error, then ok", 5, []error{refused, io.ErrUnexpectedEOF, nil}, 3, nil},
		{"gives up", 2, []error{refused, refused, refused, nil}, 3, refused},
		{"findings are no error to retry", 5, []error{errFindings}, 1, errFindings},
		{"other errors end it", 5, []error{bad, nil}, 1, bad},
		{"0 retries forever", 0, []error{refused, refused, refused, refused, refused, refused, refused, nil}, 8, nil},
	} {
		runs := 0
		err := retryCheck(context.Background(), &bytes.Buffer{}, c.retries, time.Millisecond, func(context.Context) (bool, error) {
			runs++
			return false, c.errs[runs-1]
		})
		if runs != c.wantRuns || (c.wantErr != nil && !errors.Is(err, c.wantErr)) || (c.wantErr == nil && runs == len(c.errs) && err != nil) {
			t.Errorf("%s: err %v after %d runs", c.name, err, runs)
		}
	}
}

func TestWhileLinesMatchEachConditionType(t *testing.T) {
	f := &checks.Finding{Check: "conditions", Condition: "Failed/FailureTarget"}
	line := `  ns jobs j Condition Failed/FailureTarget=True BackoffLimitExceeded "Job has reached the specified backoff limit" [conditions]`
	re := regexp.MustCompile(`Failed=True`)
	if !slices.ContainsFunc(whileLines(f, line), re.MatchString) {
		t.Errorf("Failed=True does not match %v", whileLines(f, line))
	}
	if got := whileLines(&checks.Finding{Condition: "Ready"}, "x"); len(got) != 1 {
		t.Errorf("one condition type: %v", got)
	}
}

func TestAgeWindow(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	o := checkOptions{ignoreYounger: "5m", ignoreOlder: "2026-10-30T12:00:00Z"}
	notBefore, notAfter, err := o.ageWindow(now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		f    checks.Finding
		want bool
	}{
		{checks.Finding{Check: "conditions", Since: now.Add(-time.Minute)}, false},     // too young
		{checks.Finding{Check: "conditions", Since: now.Add(-time.Hour)}, true},        // inside
		{checks.Finding{Check: "conditions", Since: now.Add(-48 * time.Hour)}, false},  // too old
		{checks.Finding{Check: "container-image-tag"}, true},                           // no start time
		{checks.Finding{Check: checks.ErrorCheck, Since: now.Add(-time.Minute)}, true}, // never hidden
	} {
		if got := inAgeWindow(&c.f, notBefore, notAfter); got != c.want {
			t.Errorf("%s since %s: %v", c.f.Check, c.f.Since, got)
		}
	}
	for _, bad := range []checkOptions{{ignoreOlder: "yesterday"}, {ignoreYounger: "-5m"}} {
		if _, _, err := bad.ageWindow(now); err == nil {
			t.Errorf("want an error for %+v", bad)
		}
	}
}

func TestCheckNotesArePrintedWhenTheyChange(t *testing.T) {
	var n checkNotes
	var out bytes.Buffer
	n.print(&out, []string{"a", "b"})
	n.print(&out, []string{"a", "b"})
	n.print(&out, nil)
	n.print(&out, []string{"b"})
	if got := out.String(); got != "a\nb\nb\n" {
		t.Errorf("got %q", got)
	}
}

func TestUncoveredNotes(t *testing.T) {
	failures := []checks.Failure{
		{Check: "gateway-references-missing", Object: "app/r1", Err: errors.New("resource gateways only listed"), Uncovered: true},
		{Check: "gateway-references-missing", Object: "app/r2", Err: errors.New("resource gateways only listed"), Uncovered: true},
		{Check: "gateway-references-missing", Object: "other/r3", Err: errors.New("x"), Uncovered: true}, // outside -n
		{Check: "conditions", Object: "app/p", Err: errors.New("boom")},                                  // a real failure
	}
	got := uncoveredNotes(failures, []string{"app"})
	want := []string{"check gateway-references-missing for 2 objects, e.g. app/r1: resource gateways only listed"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
}

func TestPlainNamespaces(t *testing.T) {
	if got := (&findingFilter{namespaces: []string{"a", "b", "a"}}).plainNamespaces(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("got %v", got)
	}
	if got := (&findingFilter{namespaces: []string{"a", "team-*"}}).plainNamespaces(); got != nil {
		t.Errorf("glob: got %v", got)
	}
}
