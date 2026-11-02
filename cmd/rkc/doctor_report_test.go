package main

import (
	"bytes"
	"errors"
	"testing"
)

type doctorFailingWriter struct {
	writes int
	failAt int
	err    error
}

func (writer *doctorFailingWriter) Write(p []byte) (int, error) {
	writer.writes++
	if writer.writes == writer.failAt {
		return 0, writer.err
	}
	return len(p), nil
}

func TestDoctorReportPropagatesWriteFailures(t *testing.T) {
	checks := []doctorCheck{{Name: "example", Status: "fail", Detail: "unavailable", Remediation: "try again"}}
	sentinel := errors.New("output unavailable")
	for _, failAt := range []int{1, 2, 3} {
		writer := &doctorFailingWriter{failAt: failAt, err: sentinel}
		if err := writeDoctorReport(writer, checks, 0, 1, true, false); !errors.Is(err, sentinel) {
			t.Fatalf("write %d: got error %v, want %v", failAt, err, sentinel)
		}
		if writer.writes != failAt {
			t.Fatalf("write %d: continued writing after failure", failAt)
		}
	}
}

func TestDoctorReportKeepsHumanReadableOutput(t *testing.T) {
	checks := []doctorCheck{{Name: "example", Status: "fail", Detail: "unavailable", Remediation: "try again"}}
	var output bytes.Buffer
	if err := writeDoctorReport(&output, checks, 0, 1, true, false); err != nil {
		t.Fatal(err)
	}
	want := "WARN  example                  unavailable\n" +
		"      fix                      try again\n" +
		"Result: fatal=0 warnings=1 ok=true strict=false\n"
	if output.String() != want {
		t.Fatalf("report = %q, want %q", output.String(), want)
	}
}
