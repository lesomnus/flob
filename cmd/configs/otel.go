package configs

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/flob/cmd/flob/version"
	"github.com/lesomnus/mkot"
	"github.com/lesomnus/otx"
	"github.com/lesomnus/z"
	"go.opentelemetry.io/otel/attribute"
	nooplog "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	nooptrace "go.opentelemetry.io/otel/trace/noop"

	_ "github.com/lesomnus/mkot/otlp"
	"github.com/lesomnus/mkot/pretty"
	_ "github.com/lesomnus/mkot/pretty"
)

type OtelConfig struct {
	mkot.Config `yaml:",inline"`
}

func (c *OtelConfig) Build(ctx context.Context) (context.Context, *otx.Otx, error) {
	otc := mkot.NewConfig()
	if c != nil {
		otc = &c.Config
	}

	const ServiceResourceId mkot.Id = "resource/flob"
	if otc.Processors == nil {
		otc.Processors = map[mkot.Id]mkot.ProcessorConfig{}
	}
	if otc.Exporters == nil {
		otc.Exporters = map[mkot.Id]mkot.ExporterConfig{}
	}
	if otc.Processors == nil {
		otc.Processors = map[mkot.Id]mkot.ProcessorConfig{}
	}
	if otc.Providers == nil {
		otc.Providers = map[mkot.Id]*mkot.ProviderConfig{}
	}
	otc.Processors[ServiceResourceId] = &mkot.Resource{
		Attributes: []mkot.Attr{
			{Key: "service.name", Value: attribute.StringValue("flob")},
			{Key: "service.version", Value: attribute.StringValue(version.Get().Version)},
		},
	}
	if len(otc.Providers) == 0 {
		id := mkot.Id("pretty")
		if _, ok := otc.Exporters[id]; !ok {
			otc.Exporters[id] = pretty.ExporterConfig{}
		}
		otc.Providers["logger"] = &mkot.ProviderConfig{
			Exporters: []mkot.Id{id},
		}
	}

	for k := range otc.Providers {
		otc.Providers[k].Processors = append(otc.Providers[k].Processors, ServiceResourceId)
	}

	resolver := mkot.Make(ctx, otc)

	tracker_provider, err := resolver.Tracer(ctx, "")
	if err != nil {
		if !errors.Is(err, mkot.ErrNotExist) {
			return nil, nil, z.Err(err, "resolve tracer provider")
		}
		tracker_provider = nooptrace.NewTracerProvider()
	}

	meter_provider, err := resolver.Meter(ctx, "")
	if err != nil {
		if !errors.Is(err, mkot.ErrNotExist) {
			return nil, nil, z.Err(err, "resolve meter provider")
		}
		meter_provider = noopmetric.NewMeterProvider()
	}

	logger_provider, err := resolver.Logger(ctx, "")
	if err != nil {
		if !errors.Is(err, mkot.ErrNotExist) {
			return nil, nil, z.Err(err, "resolve logger provider")
		}
		logger_provider = nooplog.NewLoggerProvider()
	}
	v := otx.New(
		otx.WithController(resolver),
		otx.WithTracerProvider(tracker_provider),
		otx.WithMeterProvider(meter_provider),
		otx.WithLoggerProvider(logger_provider),
	)
	return otx.Into(ctx, v), v, nil
}

var (
	_ flob.Stores = StoresTrace{}
	_ flob.Store  = StoreTrace{}
)

type StoresTrace struct {
	flob.Stores
}

func (t StoresTrace) Use(id string) flob.Store {
	return StoreTrace{t.Stores.Use(id)}
}

type StoreTrace struct {
	flob.Store
}

func (t StoreTrace) Add(ctx context.Context, m flob.Meta, r io.Reader) (flob.Meta, error) {
	ctx, span := otx.TraceStart(ctx, "add")
	defer span.End()

	return t.Store.Add(ctx, m, r)
}

func (t StoreTrace) Erase(ctx context.Context, d flob.Digest) error {
	ctx, span := otx.TraceStart(ctx, "erase")
	defer span.End()

	return t.Store.Erase(ctx, d)
}

func (t StoreTrace) Stat(ctx context.Context, d flob.Digest) (flob.Info, error) {
	ctx, span := otx.TraceStart(ctx, "stat")
	defer span.End()

	return t.Store.Stat(ctx, d)
}

func (t StoreTrace) Label(ctx context.Context, d flob.Digest, labels flob.Labels) error {
	ctx, span := otx.TraceStart(ctx, "label")
	defer span.End()

	return t.Store.Label(ctx, d, labels)
}

func (t StoreTrace) Open(ctx context.Context, d flob.Digest) (io.ReadSeekCloser, flob.Info, error) {
	ctx, span := otx.TraceStart(ctx, "open")
	defer span.End()

	return t.Store.Open(ctx, d)
}

// Unwrap exposes the wrapped store so optional capabilities (e.g. flob.Presigner)
// stay discoverable through this decorator; see flob.AsPresigner.
func (t StoreTrace) Unwrap() flob.Store {
	return t.Store
}

var (
	_ flob.Stores = StoresMeter{}
	_ flob.Store  = StoreMeter{}
)

type StoresMeter struct {
	base flob.Stores

	count    metric.Int64Counter
	duration metric.Float64Histogram
	inflight metric.Int64UpDownCounter
}

func NewStoresMeter(ctx context.Context, base flob.Stores) (flob.Stores, error) {
	meter := otx.Meter(ctx)

	s := StoresMeter{base: base}

	var err error
	errs := []error{}

	s.count, err = meter.Int64Counter("flob.store.operation.count",
		metric.WithDescription("Total number of operations performed on stores"),
		metric.WithUnit("1"),
	)
	errs = append(errs, z.ErrIf(err, "create operations counter"))

	s.duration, err = meter.Float64Histogram("flob.store.operation.duration",
		metric.WithDescription("Duration of operations performed on stores"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(
			0.001,
			0.005,
			0.01,
			0.05,
			0.1,
			0.25,
			0.5,
			1,
			2,
			5,
			10,
			30,
		),
	)
	errs = append(errs, z.ErrIf(err, "create operations duration histogram"))

	s.inflight, err = meter.Int64UpDownCounter("flob.store.operation.inflight",
		metric.WithDescription("Number of inflight operations on stores"),
		metric.WithUnit("1"),
	)
	errs = append(errs, z.ErrIf(err, "create operations inflight counter"))

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return s, nil
}

func (s StoresMeter) Use(id string) flob.Store {
	return StoreMeter{
		Store: s.base.Use(id),
		meter: s,
	}
}

type StoreMeter struct {
	flob.Store
	meter StoresMeter
}

func (s StoreMeter) Add(ctx context.Context, m flob.Meta, r io.Reader) (v flob.Meta, err error) {
	done := s.measure(ctx, "add", &err)
	defer done()

	return s.Store.Add(ctx, m, r)
}

func (s StoreMeter) Erase(ctx context.Context, d flob.Digest) (err error) {
	done := s.measure(ctx, "erase", &err)
	defer done()

	return s.Store.Erase(ctx, d)
}

func (s StoreMeter) Stat(ctx context.Context, d flob.Digest) (v flob.Info, err error) {
	done := s.measure(ctx, "stat", &err)
	defer done()

	return s.Store.Stat(ctx, d)
}

func (s StoreMeter) Label(ctx context.Context, d flob.Digest, labels flob.Labels) (err error) {
	done := s.measure(ctx, "label", &err)
	defer done()

	return s.Store.Label(ctx, d, labels)
}

func (s StoreMeter) Open(ctx context.Context, d flob.Digest) (r io.ReadSeekCloser, v flob.Info, err error) {
	done := s.measure(ctx, "open", &err)
	defer done()

	return s.Store.Open(ctx, d)
}

// Unwrap exposes the wrapped store so optional capabilities (e.g. flob.Presigner)
// stay discoverable through this decorator; see flob.AsPresigner.
func (s StoreMeter) Unwrap() flob.Store {
	return s.Store
}

func (s StoreMeter) measure(ctx context.Context, op string, errp *error) func() {
	op_attr := attribute.String("operation", op)

	inflight_attrs := metric.WithAttributes(op_attr)
	s.meter.inflight.Add(ctx, 1, inflight_attrs)
	defer s.meter.inflight.Add(ctx, -1, inflight_attrs)

	t0 := time.Now()
	return func() {
		dt := time.Since(t0).Seconds()

		attrs := []attribute.KeyValue{op_attr}
		if err := *errp; err == nil {
			attrs = append(attrs,
				attribute.String("result", "success"),
			)
		} else {
			attrs = append(attrs,
				attribute.String("result", "error"),
				attribute.String("error.type", s.errType(err)),
			)
		}

		attrs_ := metric.WithAttributes(attrs...)
		s.meter.count.Add(ctx, 1, attrs_)
		s.meter.duration.Record(ctx, dt, attrs_)
	}
}

func (StoreMeter) errType(err error) string {
	switch {
	case errors.Is(err, flob.ErrUnimplemented):
		return "unimplemented"
	case errors.Is(err, flob.ErrNotExist):
		return "not_exist"
	case errors.Is(err, flob.ErrAlreadyExists):
		return "already_exists"
	case errors.Is(err, flob.ErrInvalidDigest):
		return "invalid_digest"
	case errors.Is(err, flob.ErrDigestMismatch):
		return "digest_mismatch"
	default:
		return "unknown"
	}
}
