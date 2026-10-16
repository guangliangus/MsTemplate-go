package httpx

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"

	"example.com/msvcservice/pkg/apperror"
)

var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	// Name fields the way the wire does: the json tag, so "items[0].sku" in the errors map is what
	// the client sent, not the Go field.
	v.RegisterTagNameFunc(func(f reflect.StructField) string {
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})
	return v
}

// Validator exposes the shared instance for custom rule registration at startup.
func Validator() *validator.Validate { return validate }

// Validate checks v's validate tags and returns a 400 VALIDATION_FAILED carrying every failure
// as field -> messages, the platform's error contract shape (docs/architecture.md).
func Validate(v any) error {
	err := validate.Struct(v)
	if err == nil {
		return nil
	}
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		return apperror.BadRequest("The request could not be validated.").WithCause(err)
	}
	fields := make(map[string][]string, len(verrs))
	for _, fe := range verrs {
		path := fe.Namespace()
		if i := strings.Index(path, "."); i >= 0 {
			path = path[i+1:]
		}
		fields[path] = append(fields[path], message(fe))
	}
	return apperror.Validation(fields)
}

func message(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "is required"
	case "min":
		return "must be at least " + fe.Param()
	case "max":
		return "must be at most " + fe.Param()
	case "gt":
		return "must be greater than " + fe.Param()
	case "gte":
		return "must be at least " + fe.Param()
	case "lt":
		return "must be less than " + fe.Param()
	case "lte":
		return "must be at most " + fe.Param()
	case "oneof":
		return "must be one of: " + fe.Param()
	case "email":
		return "must be a valid email address"
	case "url":
		return "must be a valid URL"
	case "uuid":
		return "must be a UUID"
	default:
		return fmt.Sprintf("failed the '%s' rule", fe.Tag())
	}
}
