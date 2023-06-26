package playwright

import (
	"fmt"
	"path"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/danwakefield/fnmatch"
)

type (
	routeHandler = func(Route)
)

func skipFieldSerialization(val reflect.Value) bool {
	typ := val.Type()
	return (typ.Kind() == reflect.Ptr ||
		typ.Kind() == reflect.Interface ||
		typ.Kind() == reflect.Map ||
		typ.Kind() == reflect.Slice) && val.IsNil() || (val.Kind() == reflect.Interface && val.Elem().Kind() == reflect.Ptr && val.Elem().IsNil())
}

func transformStructValues(in interface{}) interface{} {
	v := reflect.ValueOf(in)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if _, ok := in.(*channel); ok {
		return in
	}
	if v.Kind() == reflect.Map || v.Kind() == reflect.Struct {
		return transformStructIntoMapIfNeeded(in)
	}
	if v.Kind() == reflect.Slice {
		outSlice := []interface{}{}
		for i := 0; i < v.Len(); i++ {
			if !skipFieldSerialization(v.Index(i)) {
				outSlice = append(outSlice, transformStructValues(v.Index(i).Interface()))
			}
		}
		return outSlice
	}
	if v.Interface() == Null() || (v.Kind() == reflect.String && v.String() == Null().(string)) {
		return "null"
	}
	return in
}

func transformStructIntoMapIfNeeded(inStruct interface{}) map[string]interface{} {
	out := make(map[string]interface{})
	v := reflect.ValueOf(inStruct)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	typ := v.Type()
	if v.Kind() == reflect.Struct {
		// Merge into the base map by the JSON struct tag
		for i := 0; i < v.NumField(); i++ {
			fi := typ.Field(i)
			// Skip the values when the field is a pointer (like *string) and nil.
			if !skipFieldSerialization(v.Field(i)) {
				// We use the JSON struct fields for getting the original names
				// out of the field.
				tagv := fi.Tag.Get("json")
				key := strings.Split(tagv, ",")[0]
				if key == "" {
					key = fi.Name
				}
				out[key] = transformStructValues(v.Field(i).Interface())
			}
		}
	} else if v.Kind() == reflect.Map {
		// Merge into the base map
		for _, key := range v.MapKeys() {
			if !skipFieldSerialization(v.MapIndex(key)) {
				out[key.String()] = transformStructValues(v.MapIndex(key).Interface())
			}
		}
	}
	return out
}

// transformOptions handles the parameter data transformation
func transformOptions(options ...interface{}) map[string]interface{} {
	var base map[string]interface{}
	var option interface{}
	// Case 1: No options are given
	if len(options) == 0 {
		return make(map[string]interface{})
	}
	if len(options) == 1 {
		// Case 2: a single value (either struct or map) is given.
		base = make(map[string]interface{})
		option = options[0]
	} else if len(options) == 2 {
		// Case 3: two values are given. The first one needs to be a map and the
		// second one can be a struct or map. It will be then get merged into the first
		// base map.
		base = transformStructIntoMapIfNeeded(options[0])
		option = options[1]
	}
	v := reflect.ValueOf(option)
	if v.Kind() == reflect.Slice {
		if v.Len() == 0 {
			return base
		}
		option = v.Index(0).Interface()
	}

	if option == nil {
		return base
	}
	v = reflect.ValueOf(option)

	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	optionMap := transformStructIntoMapIfNeeded(v.Interface())
	for key, value := range optionMap {
		base[key] = value
	}
	return base
}

func remapValue(inMapValue reflect.Value, outStructValue reflect.Value) {
	switch outStructValue.Type().Kind() {
	case reflect.Bool:
		outStructValue.SetBool(inMapValue.Bool())
	case reflect.String:
		outStructValue.SetString(inMapValue.String())
	case reflect.Float64:
		outStructValue.SetFloat(inMapValue.Float())
	case reflect.Int:
		outStructValue.SetInt(int64(inMapValue.Float()))
	case reflect.Slice:
		outStructValue.Set(reflect.MakeSlice(outStructValue.Type(), inMapValue.Len(), inMapValue.Cap()))
		for i := 0; i < inMapValue.Len(); i++ {
			remapValue(inMapValue.Index(i).Elem(), outStructValue.Index(i))
		}
	case reflect.Struct:
		structTyp := outStructValue.Type()
		for i := 0; i < outStructValue.NumField(); i++ {
			fi := structTyp.Field(i)
			key := strings.Split(fi.Tag.Get("json"), ",")[0]
			structField := outStructValue.Field(i)
			structFieldDeref := outStructValue.Field(i)
			if structField.Type().Kind() == reflect.Ptr {
				structField.Set(reflect.New(structField.Type().Elem()))
				structFieldDeref = structField.Elem()
			}
			for _, e := range inMapValue.MapKeys() {
				if key == e.String() {
					value := inMapValue.MapIndex(e)
					remapValue(value.Elem(), structFieldDeref)
				}
			}
		}
	default:
		panic(inMapValue.Interface())
	}
}

func remapMapToStruct(inputMap interface{}, outStruct interface{}) {
	remapValue(reflect.ValueOf(inputMap), reflect.ValueOf(outStruct).Elem())
}

type urlMatcher struct {
	urlOrPredicate interface{}
}

func newURLMatcher(urlOrPredicate, baseURL interface{}) *urlMatcher {
	if baseURL != nil {
		url, ok := urlOrPredicate.(string)
		if ok && !strings.HasPrefix(url, "*") {
			base, ok := baseURL.(*string)
			if ok && base != nil {
				url = path.Join(*base, url)
				return &urlMatcher{
					urlOrPredicate: url,
				}
			}
		}
	}
	return &urlMatcher{
		urlOrPredicate: urlOrPredicate,
	}
}

func (u *urlMatcher) Matches(url string) bool {
	switch v := u.urlOrPredicate.(type) {
	case *regexp.Regexp:
		return v.MatchString(url)
	case string:
		return fnmatch.Match(v, url, 0)
	}
	if reflect.TypeOf(u.urlOrPredicate).Kind() == reflect.Func {
		function := reflect.ValueOf(u.urlOrPredicate)
		result := function.Call([]reflect.Value{reflect.ValueOf(url)})
		return result[0].Bool()
	}
	panic(u.urlOrPredicate)
}

type routeHandlerEntry struct {
	matcher *urlMatcher
	handler routeHandler
	times   int
	count   int32
}

func (r *routeHandlerEntry) Matches(url string) bool {
	return r.matcher.Matches(url)
}

func (r *routeHandlerEntry) Handle(route Route) chan bool {
	handled := route.(*routeImpl).startHandling()
	atomic.AddInt32(&r.count, 1)
	r.handler(route)
	return handled
}

func (r *routeHandlerEntry) WillExceed() bool {
	if r.times == 0 {
		return false
	}
	return int(atomic.LoadInt32(&r.count)+1) >= r.times
}

func newRouteHandlerEntry(matcher *urlMatcher, handler routeHandler, times ...int) *routeHandlerEntry {
	n := 0
	if len(times) > 0 {
		n = times[0]
	}
	return &routeHandlerEntry{
		matcher: matcher,
		handler: handler,
		times:   n,
		count:   0,
	}
}

type safeStringSet struct {
	sync.Mutex
	v []string
}

func (s *safeStringSet) Has(expected string) bool {
	s.Lock()
	defer s.Unlock()
	for _, v := range s.v {
		if v == expected {
			return true
		}
	}
	return false
}

func (s *safeStringSet) Add(v string) {
	if s.Has(v) {
		return
	}
	s.Lock()
	s.v = append(s.v, v)
	s.Unlock()
}

func (s *safeStringSet) Remove(remove string) {
	s.Lock()
	defer s.Unlock()
	newSlice := make([]string, 0)
	for _, v := range s.v {
		if v != remove {
			newSlice = append(newSlice, v)
		}
	}
	if len(s.v) != len(newSlice) {
		s.v = newSlice
	}
}

func newSafeStringSet(v []string) *safeStringSet {
	return &safeStringSet{
		v: v,
	}
}

const defaultTimeout = 30 * 1000

type timeoutSettings struct {
	parent            *timeoutSettings
	timeout           float64
	navigationTimeout float64
}

func (t *timeoutSettings) SetTimeout(timeout float64) {
	t.timeout = timeout
}

func (t *timeoutSettings) Timeout() float64 {
	if t.timeout != 0 {
		return t.timeout
	}
	if t.parent != nil {
		return t.parent.Timeout()
	}
	return defaultTimeout
}

func (t *timeoutSettings) SetNavigationTimeout(navigationTimeout float64) {
	t.navigationTimeout = navigationTimeout
}

func (t *timeoutSettings) NavigationTimeout() float64 {
	if t.navigationTimeout != 0 {
		return t.navigationTimeout
	}
	if t.parent != nil {
		return t.parent.NavigationTimeout()
	}
	return defaultTimeout
}

func newTimeoutSettings(parent *timeoutSettings) *timeoutSettings {
	return &timeoutSettings{
		parent:            parent,
		timeout:           defaultTimeout,
		navigationTimeout: defaultTimeout,
	}
}

// SelectOptionValues is the option struct for ElementHandle.Select() etc.
type SelectOptionValues struct {
	ValuesOrLabels *[]string
	Values         *[]string
	Indexes        *[]int
	Labels         *[]string
	Elements       *[]ElementHandle
}

func convertSelectOptionSet(values SelectOptionValues) map[string]interface{} {
	out := make(map[string]interface{})
	if values == (SelectOptionValues{}) {
		return out
	}

	var o []map[string]interface{}
	if values.ValuesOrLabels != nil {
		for _, v := range *values.ValuesOrLabels {
			m := map[string]interface{}{"valueOrLabel": v}
			o = append(o, m)
		}
	}
	if values.Values != nil {
		for _, v := range *values.Values {
			m := map[string]interface{}{"value": v}
			o = append(o, m)
		}
	}
	if values.Indexes != nil {
		for _, i := range *values.Indexes {
			m := map[string]interface{}{"index": i}
			o = append(o, m)
		}
	}
	if values.Labels != nil {
		for _, l := range *values.Labels {
			m := map[string]interface{}{"label": l}
			o = append(o, m)
		}
	}
	if o != nil {
		out["options"] = o
	}

	var e []*channel
	if values.Elements != nil {
		for _, eh := range *values.Elements {
			e = append(e, eh.(*elementHandleImpl).channel)
		}
	}
	if e != nil {
		out["elements"] = e
	}

	return out
}

func unroute(channel *channel, inRoutes []*routeHandlerEntry, url interface{}, handlers ...routeHandler) ([]*routeHandlerEntry, error) {
	var handler routeHandler
	if len(handlers) == 1 {
		handler = handlers[0]
	}
	handlerPtr := reflect.ValueOf(handler).Pointer()

	routes := make([]*routeHandlerEntry, 0)

	for _, route := range inRoutes {
		routeHandlerPtr := reflect.ValueOf(route.handler).Pointer()
		if route.matcher.urlOrPredicate != url ||
			(handler != nil && routeHandlerPtr != handlerPtr) {
			routes = append(routes, route)
		}
	}

	if len(routes) == 0 {
		_, err := channel.Send("setNetworkInterceptionEnabled", map[string]interface{}{
			"enabled": false,
		})
		if err != nil {
			return nil, err
		}
	}
	return routes, nil
}

func serializeMapToNameAndValue(headers map[string]string) []map[string]string {
	serialized := make([]map[string]string, 0)
	for name, value := range headers {
		serialized = append(serialized, map[string]string{
			"name":  name,
			"value": value,
		})
	}
	return serialized
}

// assignStructFields assigns fields from src to dest,
//
//	omitExtra determines whether to omit src's extra fields
func assignStructFields(dest, src interface{}, omitExtra bool) error {
	destValue := reflect.ValueOf(dest)
	if destValue.Kind() != reflect.Ptr || destValue.IsNil() {
		return fmt.Errorf("dest must be a non-nil pointer")
	}
	destValue = destValue.Elem()
	if destValue.Kind() != reflect.Struct {
		return fmt.Errorf("dest must be a struct")
	}

	srcValue := reflect.ValueOf(src)
	if srcValue.Kind() == reflect.Ptr {
		srcValue = srcValue.Elem()
	}
	if srcValue.Kind() != reflect.Struct {
		return fmt.Errorf("src must be a struct")
	}

	for i := 0; i < destValue.NumField(); i++ {
		destField := destValue.Field(i)
		destFieldType := destField.Type()
		destFieldName := destValue.Type().Field(i).Name

		if srcField := srcValue.FieldByName(destFieldName); srcField.IsValid() && srcField.Type() != destFieldType {
			return fmt.Errorf("mismatched field type for field %s", destFieldName)
		} else if srcField.IsValid() {
			destField.Set(srcField)
		}
	}

	if !omitExtra {
		for i := 0; i < srcValue.NumField(); i++ {
			srcFieldName := srcValue.Type().Field(i).Name

			if destField := destValue.FieldByName(srcFieldName); !destField.IsValid() {
				return fmt.Errorf("extra field %s in src", srcFieldName)
			}
		}
	}

	return nil
}

func deserializeNameAndValueToMap(headersArray []map[string]string) map[string]string {
	unserialized := make(map[string]string)
	for _, item := range headersArray {
		unserialized[item["name"]] = item["value"]
	}
	return unserialized
}

type recordHarOptions struct {
	Path           string            `json:"path"`
	Content        *HarContentPolicy `json:"content,omitempty"`
	Mode           *HarMode          `json:"mode,omitempty"`
	UrlGlob        *string           `json:"urlGlob,omitempty"`
	UrlRegexSource *string           `json:"urlRegexSource,omitempty"`
	UrlRegexFlags  *string           `json:"urlRegexFlags,omitempty"`
}

type recordHarInputOptions struct {
	Path        string
	URL         interface{}
	Mode        *HarMode
	Content     *HarContentPolicy
	OmitContent *bool
}

type harRecordingMetadata struct {
	Path    string
	Content *HarContentPolicy
}

func prepareRecordHarOptions(option recordHarInputOptions) recordHarOptions {
	out := recordHarOptions{
		Path: option.Path,
	}
	if option.URL != nil {
		switch option.URL.(type) {
		case *regexp.Regexp:
			pattern, flags := convertRegexp(option.URL.(*regexp.Regexp))
			out.UrlRegexSource = String(pattern)
			out.UrlRegexFlags = String(flags)
		case string:
			out.UrlGlob = String(option.URL.(string))
		}
	}
	if option.Mode != nil {
		out.Mode = option.Mode
	}
	if option.Content != nil {
		out.Content = option.Content
	} else if option.OmitContent != nil && *option.OmitContent {
		out.Content = HarContentPolicyOmit
	}
	return out
}
