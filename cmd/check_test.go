package cmd

import (
	"bytes"
	"context"
	"errors"
	"net"
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
			return false, &net.OpError{Op: "dial", Err: errors.New("connection refused")}
		case 3:
			return false, errors.New("list pods: the server is currently unable to handle the request") // retried after a successful run
		case 4:
			return true, nil
		}
		return false, errFindings // findings, but none matches
	})
	if err != nil || runs != 5 {
		t.Fatalf("err %v after %d runs", err, runs)
	}
	if got := strings.Count(msg.String(), "Trying again"); got != 2 {
		t.Errorf("%d retries in:\n%s", got, msg.String())
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

func TestRepeatCheckNetworkErrorsAreRetriedFromTheStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runs := 0
	err := repeatCheck(ctx, &bytes.Buffer{}, time.Millisecond, false, func(context.Context) (bool, error) {
		runs++
		if runs == 3 {
			cancel()
		}
		return false, &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	})
	if !errors.Is(err, context.Canceled) || runs != 3 {
		t.Errorf("err %v after %d runs", err, runs)
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
	if _, _, err := (&checkOptions{ignoreOlder: "yesterday"}).ageWindow(now); err == nil {
		t.Error("want an error")
	}
}
