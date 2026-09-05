package knowledge_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/knowledge"
)

// referenceKinds are the field kinds through which a value handed to a
// ranker still shares state with the caller's own. A field of one of
// these kinds is a write channel unless CloneForRanking copies it.
var referenceKinds = map[reflect.Kind]bool{
	reflect.Slice:     true,
	reflect.Map:       true,
	reflect.Pointer:   true,
	reflect.Chan:      true,
	reflect.Func:      true,
	reflect.Interface: true,
}

// clonedFields are the reference-typed fields CloneForRanking is known to
// copy, verified behaviourally by TestCloneForRanking_SeversEveryWriteChannel.
// Adding a field here without also copying it in CloneForRanking makes
// that test fail, so the two cannot drift apart silently.
var clonedFields = map[string]map[string]bool{
	"Candidate": {"Tags": true, "Roots": true},
	"Query":     {"Tags": true, "Roots": true, "ChangedPaths": true},
}

// TestCloneForRanking_CoversEveryReferenceTypedField is a forward guard,
// not a check of today's behaviour.
//
// The copy is complete right now. The risk is the next field: add
// `Metadata map[string]string` to Candidate and CloneForRanking keeps
// compiling, keeps passing every existing test, and silently stops being
// deep -- reopening the substitution channel it exists to close, with no
// signal at all. This test turns that into a CI failure at the moment the
// field is added, which is the only moment anyone is thinking about it.
//
// time.Time is exempt: it carries a *Location, but a Candidate's
// timestamps are copied by value and a ranker mutating its own copy
// cannot reach the caller's.
func TestCloneForRanking_CoversEveryReferenceTypedField(t *testing.T) {
	t.Parallel()

	timeType := reflect.TypeOf(time.Time{})

	for _, typ := range []reflect.Type{
		reflect.TypeOf(knowledge.Candidate{}),
		reflect.TypeOf(knowledge.Query{}),
	} {
		name := typ.Name()
		known, ok := clonedFields[name]
		if !ok {
			t.Fatalf("%s has no entry in clonedFields -- add one, or this guard silently covers nothing", name)
		}

		seen := 0
		for i := range typ.NumField() {
			f := typ.Field(i)
			if f.Type == timeType {
				continue
			}
			if !referenceKinds[f.Type.Kind()] {
				continue
			}
			seen++
			if !known[f.Name] {
				t.Errorf("%s.%s is a %s -- a ranker holding it shares state with the caller. "+
					"Copy it in CloneForRanking and add it to clonedFields, or state here why it cannot be written through.",
					name, f.Name, f.Type.Kind())
			}
		}

		for fieldName := range known {
			if _, found := typ.FieldByName(fieldName); !found {
				t.Errorf("clonedFields[%q] names %q, which %s no longer has -- the guard is describing a field that is gone", name, fieldName, name)
			}
		}
		if seen == 0 {
			t.Errorf("%s: found no reference-typed fields at all -- the guard is vacuous", name)
		}
	}
}
