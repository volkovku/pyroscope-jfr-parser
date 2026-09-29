package pprof

import (
	"strings"

	"github.com/grafana/jfr-parser/parser"
	"github.com/grafana/jfr-parser/parser/types"
)

const (
	sampleTypeCPU         = 0
	sampleTypeWall        = 1
	sampleTypeInTLAB      = 2
	sampleTypeOutTLAB     = 3
	sampleTypeLock        = 4
	sampleTypeThreadPark  = 5
	sampleTypeLiveObject  = 6
	sampleTypeAllocSample = 7
	sampleTypeMalloc      = 8
)

func newJfrPprofBuilders(p *parser.Parser, jfrLabels *LabelsSnapshot, piOriginal *ParseInput, opt *pprofOptions) *jfrPprofBuilders {
	st := piOriginal.StartTime.UnixNano()
	et := piOriginal.EndTime.UnixNano()
	var period int64
	if piOriginal.SampleRate == 0 {
		period = 0
	} else {
		period = 1e9 / int64(piOriginal.SampleRate)
	}

	res := &jfrPprofBuilders{
		parser:        p,
		builders:      make(map[int64]*ProfileBuilder),
		jfrLabels:     jfrLabels,
		timeNanos:     st,
		durationNanos: et - st,
		period:        period,
		opt:           opt,
	}
	return res
}

type jfrPprofBuilders struct {
	parser        *parser.Parser
	builders      map[int64]*ProfileBuilder
	jfrLabels     *LabelsSnapshot
	timeNanos     int64
	durationNanos int64
	period        int64
	opt           *pprofOptions

	javaVMName                 string
	javaVMVersion              string
	javaVMSpecificationVersion string

	metrics ParseMetrics
}

func (b *jfrPprofBuilders) setProcessRuntime(name, value string) {
	switch name {
	case "java.vm.name":
		b.javaVMName = strings.Clone(value)
	case "java.vm.version":
		b.javaVMVersion = strings.Clone(value)
	case "java.vm.specification.version":
		b.javaVMSpecificationVersion = strings.Clone(value)
	}
}

func javaMajorVersion(specificationVersion string) string {
	version := specificationVersion
	if strings.HasPrefix(version, "1.") {
		version = strings.TrimPrefix(version, "1.")
	}
	return version
}

func (b *jfrPprofBuilders) addStacktrace(sampleType int64, correlation StacktraceCorrelation, ref types.StackTraceRef, values []int64, classRef types.ClassRef) {
	p := b.profileBuilderForSampleType(sampleType)
	st := b.parser.GetStacktrace(ref)
	if st == nil {
		b.metrics.StacktraceNotFound++
		return
	}

	addValues := func(dst []int64) {
		mul := 1
		if sampleType == sampleTypeCPU || sampleType == sampleTypeWall {
			mul = int(b.period)
		}
		for i, value := range values {
			dst[i] += value * int64(mul)
		}
	}

	// Combine stackTraceRef + classRef into a unique locationsID so that the same
	// call stack allocating different types produces separate pprof samples.
	locationsID := uint64(ref)
	if classRef != 0 {
		locationsID ^= uint64(classRef) * 0x9e3779b97f4a7c15
	}

	sample := p.FindExternalSampleWithCorrelation(locationsID, correlation)
	if sample != nil {
		addValues(sample.Value)
		return
	}

	nLocs := len(st.Frames)
	if classRef != 0 {
		nLocs++
	}
	if b.opt.truncatedFrame && st.Truncated {
		nLocs++
	}
	locations := make([]uint64, 0, nLocs)

	// Prepend the allocated class as the innermost (leaf) frame.
	if classRef != 0 {
		if classLocID := b.classLocation(p, classRef); classLocID != 0 {
			locations = append(locations, uint64(classLocID))
		}
	}

	for i := 0; i < len(st.Frames); i++ {
		f := st.Frames[i]
		extLocID := ExternalLocationID{
			ExternalFunctionID: ExternalFunctionID(f.Method),
			Line:               f.LineNumber,
		}
		loc, found := p.FindLocationByExternalID(extLocID)
		if found {
			locations = append(locations, uint64(loc))
			continue
		}
		m := b.parser.GetMethod(f.Method)
		if m != nil {

			pprofFuncID, found := p.FindFunctionByExternalID(extLocID.ExternalFunctionID)
			if found {
				// add new location with old function
			} else {
				cls := b.parser.GetClass(m.Type)
				if cls == nil {
					b.metrics.ClassNotFound++
					continue
				}
				clsName := b.parser.GetSymbolString(cls.Name)
				methodName := b.parser.GetSymbolString(m.Name)
				frame := clsName + "." + methodName
				pprofFuncID = p.AddExternalFunction(frame, extLocID.ExternalFunctionID)
			}
			loc = p.AddExternalLocation(extLocID, pprofFuncID)
			locations = append(locations, uint64(loc))
		} else {
			b.metrics.MethodNotFound++
		}
	}
	if b.opt.truncatedFrame && st.Truncated {
		locations = append(locations, p.getTruncatedLocation())
	}
	if correlation.ThreadName != "" {
		locations = append(locations, p.getThreadLocation(correlation.ThreadName))
	}
	vs := make([]int64, len(values))
	addValues(vs)
	p.AddExternalSampleWithLabels(locations, vs, b.contextLabels(correlation.ContextId), b.jfrLabels, locationsID, correlation)
}

// classLocation returns (or creates) a pprof location for the allocated class frame.
// Bit 63 is set in the ExternalFunctionID namespace to avoid collisions with method IDs.
func (b *jfrPprofBuilders) classLocation(p *ProfileBuilder, classRef types.ClassRef) PPROFLocationID {
	extFuncID := ExternalFunctionID(uint64(classRef) | (uint64(1) << 63))
	extLocID := ExternalLocationID{ExternalFunctionID: extFuncID, Line: 0}

	loc, found := p.FindLocationByExternalID(extLocID)
	if found {
		return loc
	}

	pprofFuncID, found := p.FindFunctionByExternalID(extFuncID)
	if !found {
		cls := b.parser.GetClass(classRef)
		if cls == nil {
			return 0
		}
		className := jvmClassToName(b.parser.GetSymbolString(cls.Name))
		pprofFuncID = p.AddExternalFunction(className, extFuncID)
	}
	return p.AddExternalLocation(extLocID, pprofFuncID)
}

// jvmClassToName converts a JVM internal class name (as stored in JFR symbols)
// to a human-readable Java class name, handling array descriptors.
// Examples: "java/lang/String" → "java.lang.String"
//
//	"[Ljava/lang/String;" → "java.lang.String[]"
//	"[I"                  → "int[]"
func jvmClassToName(symbol string) string {
	arrayDepth := 0
	for arrayDepth < len(symbol) && symbol[arrayDepth] == '[' {
		arrayDepth++
	}

	base := symbol[arrayDepth:]
	var name string

	if arrayDepth > 0 {
		switch base {
		case "B":
			name = "byte"
		case "C":
			name = "char"
		case "S":
			name = "short"
		case "I":
			name = "int"
		case "J":
			name = "long"
		case "Z":
			name = "boolean"
		case "F":
			name = "float"
		case "D":
			name = "double"
		default:
			if len(base) > 2 && base[0] == 'L' && base[len(base)-1] == ';' {
				name = strings.ReplaceAll(base[1:len(base)-1], "/", ".")
			} else {
				name = strings.ReplaceAll(base, "/", ".")
			}
		}
	} else {
		name = strings.ReplaceAll(base, "/", ".")
	}

	return name + strings.Repeat("[]", arrayDepth)
}

func (b *jfrPprofBuilders) profileBuilderForSampleType(sampleType int64) *ProfileBuilder {
	if builder, ok := b.builders[sampleType]; ok {
		return builder
	}
	builder := NewProfileBuilderWithLabels(b.timeNanos)
	builder.DurationNanos = b.durationNanos
	var metric string
	switch sampleType {
	case sampleTypeCPU:
		builder.AddSampleType("cpu", "nanoseconds")
		builder.PeriodType("cpu", "nanoseconds")
		metric = "process_cpu"
	case sampleTypeWall:
		builder.AddSampleType("wall", "nanoseconds")
		builder.PeriodType("wall", "nanoseconds")
		metric = "wall"
	case sampleTypeInTLAB:
		builder.AddSampleType("alloc_in_new_tlab_objects", "count")
		builder.AddSampleType("alloc_in_new_tlab_bytes", "bytes")
		builder.PeriodType("space", "bytes")
		metric = "memory"
	case sampleTypeOutTLAB:
		builder.AddSampleType("alloc_outside_tlab_objects", "count")
		builder.AddSampleType("alloc_outside_tlab_bytes", "bytes")
		builder.PeriodType("space", "bytes")
		metric = "memory"
	case sampleTypeLock:
		builder.AddSampleType("contentions", "count")
		builder.AddSampleType("delay", "nanoseconds")
		builder.PeriodType("mutex", "count")
		metric = "mutex"
	case sampleTypeThreadPark:
		builder.AddSampleType("contentions", "count")
		builder.AddSampleType("delay", "nanoseconds")
		builder.PeriodType("block", "count")
		metric = "block"
	case sampleTypeLiveObject:
		builder.AddSampleType("live", "count")
		builder.PeriodType("objects", "count")
		metric = "memory"
	case sampleTypeAllocSample:
		builder.AddSampleType("alloc_sample_objects", "count")
		builder.AddSampleType("alloc_sample_bytes", "bytes")
		builder.PeriodType("space", "bytes")
		metric = "memory"
	case sampleTypeMalloc:
		builder.AddSampleType("malloc_objects", "count")
		builder.AddSampleType("malloc_bytes", "bytes")
		metric = "memory"
	}
	builder.MetricName(metric)
	b.builders[sampleType] = builder
	return builder
}

func (b *jfrPprofBuilders) contextLabels(contextID uint64) *Context {
	if b.jfrLabels == nil {
		return nil
	}
	return b.jfrLabels.Contexts[int64(contextID)]
}

func (b *jfrPprofBuilders) build(jfrEvent string) *Profiles {
	profiles := make([]Profile, 0, len(b.builders))
	for _, builder := range b.builders {
		profiles = append(profiles, Profile{
			Profile: builder.Profile,
			Metric:  builder.metricName,
		})
	}
	return &Profiles{
		Profiles:                   profiles,
		JFREvent:                   jfrEvent,
		ProcessRuntimeName:         b.javaVMName,
		ProcessRuntimeVersion:      b.javaVMVersion,
		ProcessRuntimeVersionMajor: javaMajorVersion(b.javaVMSpecificationVersion),
	}
}
