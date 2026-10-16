package conf

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/go-playground/validator/v10"
)

// Validatable is implemented by configuration sections with cross-field rules the struct tags
// cannot express (stale_after must exceed task_timeout, a key must decode to 32 bytes, ...).
type Validatable interface {
	Validate() error
}

var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	// Report koanf names (postgres.dsn), not Go field names (Postgres.DSN): the operator fixing the
	// error edits YAML or an environment variable, not the struct.
	v.RegisterTagNameFunc(func(f reflect.StructField) string {
		name := strings.SplitN(f.Tag.Get(Tag), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})
	return v
}

// Validate runs the validate struct tags over cfg, then every Validate method found on cfg and on
// any nested struct, pointer, slice element or map value. All failures are reported together.
func Validate(cfg any) error {
	var errs []error
	if err := validate.Struct(cfg); err != nil {
		errs = append(errs, formatTagErrors(err)...)
	}
	errs = append(errs, walkValidatable(reflect.ValueOf(cfg), "")...)
	if len(errs) == 0 {
		return nil
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return fmt.Errorf("conf: invalid configuration:\n  %w", errors.Join(errs...))
}

func formatTagErrors(err error) []error {
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		return []error{err}
	}
	out := make([]error, 0, len(verrs))
	for _, fe := range verrs {
		// Namespace is "Config.postgres.dsn": drop the root type name.
		path := fe.Namespace()
		if i := strings.Index(path, "."); i >= 0 {
			path = path[i+1:]
		}
		msg := fmt.Sprintf("%s: failed '%s'", path, fe.Tag())
		if fe.Param() != "" {
			msg += " (" + fe.Param() + ")"
		}
		out = append(out, errors.New(msg))
	}
	return out
}

// walkValidatable visits nested values depth-first and collects Validate failures, prefixing each
// with the koanf path so the message names the section.
func walkValidatable(v reflect.Value, path string) []error {
	var errs []error
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		return walkValidatable(v.Elem(), path)
	}
	if v.CanAddr() {
		if val, ok := v.Addr().Interface().(Validatable); ok {
			if err := val.Validate(); err != nil {
				errs = append(errs, prefix(path, err))
			}
		}
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			errs = append(errs, walkValidatable(v.Field(i), join(path, tagName(field)))...)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			// Map values are not addressable; copy them so pointer-receiver Validate methods work.
			elem := reflect.New(v.Type().Elem()).Elem()
			elem.Set(v.MapIndex(key))
			errs = append(errs, walkValidatable(elem, join(path, fmt.Sprint(key.Interface())))...)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			errs = append(errs, walkValidatable(v.Index(i), fmt.Sprintf("%s[%d]", path, i))...)
		}
	default:
		// scalars carry no nested sections
	}
	return errs
}

func tagName(f reflect.StructField) string {
	name := strings.SplitN(f.Tag.Get(Tag), ",", 2)[0]
	if name == "" {
		return strings.ToLower(f.Name)
	}
	return name
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + Delim + name
}

func prefix(path string, err error) error {
	if path == "" {
		return err
	}
	return fmt.Errorf("%s: %w", path, err)
}
