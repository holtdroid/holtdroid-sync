package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/v2"
)

// Load fills dst from defaults, then the YAML file at path (optional), then
// environment variables starting with prefix. Keys are matched case-insensitively.
func Load(prefix, path string, defaults, dst any) error {
	k := koanf.New(".")

	if defaults != nil {
		m, err := structToMap(defaults)
		if err != nil {
			return err
		}
		if err := k.Load(confmap.Provider(m, "."), nil); err != nil {
			return err
		}
	}

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read config: %w", err)
		}
		m, err := yaml.Parser().Unmarshal(raw)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if err := k.Load(confmap.Provider(lowerKeys(m).(map[string]any), "."), nil); err != nil {
			return err
		}
	}

	envProvider := env.Provider(".", env.Opt{
		Prefix: prefix,
		TransformFunc: func(key, value string) (string, any) {
			key = strings.ToLower(strings.TrimPrefix(key, prefix))
			return strings.ReplaceAll(key, "__", "."), value
		},
	})
	if err := k.Load(envProvider, nil); err != nil {
		return err
	}

	return k.UnmarshalWithConf("", dst, koanf.UnmarshalConf{
		DecoderConfig: &mapstructure.DecoderConfig{
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToTimeDurationHookFunc(),
				mapstructure.StringToSliceHookFunc(","),
				mapstructure.TextUnmarshallerHookFunc(),
			),
			Result:           dst,
			WeaklyTypedInput: true,
			MatchName:        strings.EqualFold,
		},
	})
}

func structToMap(v any) (map[string]any, error) {
	m, ok := lowerKeys(normalize(v)).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("defaults must be a struct, got %T", v)
	}
	return m, nil
}

func normalize(v any) any {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		out := map[string]any{}
		for _, key := range rv.MapKeys() {
			out[fmt.Sprint(key.Interface())] = normalize(rv.MapIndex(key).Interface())
		}
		return out
	case reflect.Struct:
		var m map[string]any
		if err := mapstructure.Decode(v, &m); err != nil {
			return v
		}
		return normalize(m)
	default:
		return v
	}
}

func lowerKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[strings.ToLower(k)] = lowerKeys(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = lowerKeys(val)
		}
		return out
	default:
		return v
	}
}
