package actor

import (
	"context"
	"testing"
)

func TestFromContextDefaultsToSystem(t *testing.T) {
	if FromContext(context.Background()) != System {
		t.Fatal("no actor means system")
	}
	if FromContext(WithName(context.Background(), "42")) != "42" {
		t.Fatal("direct value")
	}
}

func TestRecorderMakesInnerActorVisibleToOuterFrame(t *testing.T) {
	outer := WithRecorder(context.Background())
	inner := WithName(outer, "7") // a derived context, as a middleware further down creates
	if FromContext(inner) != "7" {
		t.Fatal("inner frame")
	}
	if FromContext(outer) != "7" {
		t.Fatal("the outer frame sees the recorded actor")
	}
}
