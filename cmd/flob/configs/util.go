package configs

import (
	"fmt"
	"reflect"

	"github.com/go-viper/mapstructure/v2"
	"github.com/lesomnus/z"
)

type configmap map[string]any

func (m configmap) unmarshal(kind string, v any) (any, error) {
	t, ok := m[kind]
	if !ok {
		return nil, fmt.Errorf("unknown kind: %q", kind)
	}

	v_ := reflect.New(reflect.TypeOf(t)).Interface()
	if err := mapstructure.Decode(v, v_); err != nil {
		return nil, z.Err(err, "decode: %q", kind)
	}

	return v_, nil
}
