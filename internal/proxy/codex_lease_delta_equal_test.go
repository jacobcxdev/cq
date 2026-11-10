package proxy

import (
	"reflect"
	"testing"
	"time"
)

func TestCodexDeltaEqualityCoversEveryField(t *testing.T) {
	for _, schema := range []struct {
		name   string
		typeOf reflect.Type
		equal  func(reflect.Value, reflect.Value) bool
	}{
		{"lane", reflect.TypeFor[CodexJournalLane](), func(a, b reflect.Value) bool {
			return equalCodexDeltaLane(a.Interface().(*CodexJournalLane), b.Interface().(*CodexJournalLane))
		}},
		{"record", reflect.TypeFor[CodexJournalRecordV2](), func(a, b reflect.Value) bool {
			return equalCodexDeltaRecord(a.Interface().(*CodexJournalRecordV2), b.Interface().(*CodexJournalRecordV2))
		}},
	} {
		t.Run(schema.name, func(t *testing.T) {
			check := func(path string, changeA, changeB func(reflect.Value)) {
				t.Helper()
				a, b := reflect.New(schema.typeOf), reflect.New(schema.typeOf)
				fillCodexEqualitySample(t, a.Elem())
				fillCodexEqualitySample(t, b.Elem())
				if changeA != nil {
					changeA(a.Elem())
				}
				if changeB != nil {
					changeB(b.Elem())
				}
				if got, want := schema.equal(a, b), reflect.DeepEqual(a.Interface(), b.Interface()); got != want {
					t.Fatalf("equality differs from reflection at %s: got %t, want %t", path, got, want)
				}
			}
			check("identical independent values", nil, nil)
			var visit func(reflect.Type, string, func(reflect.Value) reflect.Value)
			visit = func(kind reflect.Type, path string, selectValue func(reflect.Value) reflect.Value) {
				if kind == reflect.TypeFor[time.Time]() {
					check(path, nil, func(root reflect.Value) {
						value := selectValue(root)
						value.Set(reflect.ValueOf(value.Interface().(time.Time).Add(time.Nanosecond)))
					})
					return
				}
				switch kind.Kind() {
				case reflect.Struct:
					for index := range kind.NumField() {
						visit(kind.Field(index).Type, path+"."+kind.Field(index).Name, func(root reflect.Value) reflect.Value { return selectValue(root).Field(index) })
					}
				case reflect.Slice:
					setNil := func(root reflect.Value) { selectValue(root).SetZero() }
					setEmpty := func(root reflect.Value) { selectValue(root).Set(reflect.MakeSlice(kind, 0, 0)) }
					check(path+" nil/empty", setNil, setEmpty)
					check(path+" nil/nil", setNil, setNil)
					check(path+" empty/empty", setEmpty, setEmpty)
					check(path+" length", nil, func(root reflect.Value) { value := selectValue(root); value.Set(value.Slice(0, 1)) })
					for index := range 2 {
						visit(kind.Elem(), path+" element", func(root reflect.Value) reflect.Value { return selectValue(root).Index(index) })
					}
				case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
					reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
					check(path, nil, func(root reflect.Value) {
						value := selectValue(root)
						switch value.Kind() {
						case reflect.String:
							value.SetString(value.String() + "changed")
						case reflect.Bool:
							value.SetBool(!value.Bool())
						case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
							value.SetInt(value.Int() + 1)
						default:
							value.SetUint(value.Uint() + 1)
						}
					})
				default:
					t.Fatalf("new schema field at %s needs equality coverage: %s", path, kind)
				}
			}
			visit(schema.typeOf, schema.name, func(root reflect.Value) reflect.Value { return root })
		})
	}
}

func fillCodexEqualitySample(t *testing.T, value reflect.Value) {
	t.Helper()
	if value.Type() == reflect.TypeFor[time.Time]() {
		value.Set(reflect.ValueOf(time.Unix(1234567890, 123).UTC()))
		return
	}
	switch value.Kind() {
	case reflect.Struct:
		for index := range value.NumField() {
			fillCodexEqualitySample(t, value.Field(index))
		}
	case reflect.Slice:
		value.Set(reflect.MakeSlice(value.Type(), 2, 2))
		for index := range value.Len() {
			fillCodexEqualitySample(t, value.Index(index))
		}
	case reflect.String:
		value.SetString("sample")
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(1)
	default:
		t.Fatalf("new schema field needs equality sample: %s", value.Type())
	}
}
