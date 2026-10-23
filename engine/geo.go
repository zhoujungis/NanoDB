package engine

import (
	"math"
	"sort"
	"strings"
)

const earthRadiusMeters = 6371008.8

// geoIndex 在一个字段上维护按经度排序的点，支持包围盒与圆形范围查询。
type geoIndex struct {
	field   string
	entries []geoEntry
	points  map[string][2]float64
}

type geoEntry struct {
	lon, lat float64
	id       string
}

func newGeoIndex(field string) *geoIndex {
	return &geoIndex{field: field, points: make(map[string][2]float64)}
}

func (g *geoIndex) clone() *geoIndex {
	c := newGeoIndex(g.field)
	c.entries = append([]geoEntry(nil), g.entries...)
	for id, p := range g.points {
		c.points[id] = p
	}
	return c
}

func (g *geoIndex) add(id string, doc map[string]any) {
	v, ok := getPath(doc, g.field)
	if !ok {
		return
	}
	lon, lat, ok := geoPoint(v)
	if !ok {
		return
	}
	g.remove(id)
	g.points[id] = [2]float64{lon, lat}
	pos := sort.Search(len(g.entries), func(i int) bool {
		return compareGeoEntry(g.entries[i], geoEntry{lon: lon, lat: lat, id: id}) >= 0
	})
	g.entries = append(g.entries, geoEntry{})
	copy(g.entries[pos+1:], g.entries[pos:])
	g.entries[pos] = geoEntry{lon: lon, lat: lat, id: id}
}

func compareGeoEntry(a, b geoEntry) int {
	switch {
	case a.lon < b.lon:
		return -1
	case a.lon > b.lon:
		return 1
	}
	switch {
	case a.lat < b.lat:
		return -1
	case a.lat > b.lat:
		return 1
	}
	return strings.Compare(a.id, b.id)
}

func (g *geoIndex) remove(id string) {
	p, ok := g.points[id]
	if !ok {
		return
	}
	delete(g.points, id)
	lo := sort.Search(len(g.entries), func(i int) bool { return g.entries[i].lon >= p[0] })
	for i := lo; i < len(g.entries) && g.entries[i].lon == p[0]; i++ {
		if g.entries[i].id == id {
			g.entries = append(g.entries[:i], g.entries[i+1:]...)
			return
		}
	}
}

// lookup 计算 $geoWithin 条件的候选文档，非地理条件不参与该索引。
func (g *geoIndex) lookup(conds []any) map[string]struct{} {
	var best map[string]struct{}
	for _, cond := range conds {
		ops, ok := cond.(map[string]any)
		if !ok {
			continue
		}
		spec, has := ops["$geoWithin"]
		if !has {
			continue
		}
		shape, ok := parseGeoWithin(spec)
		if !ok {
			return nil
		}
		var ids map[string]struct{}
		if shape.isBox {
			ids = g.rangeIDs(shape.minLon, shape.minLat, shape.maxLon, shape.maxLat)
		} else {
			minLon, minLat, maxLon, maxLat := circleBounds(shape.lon, shape.lat, shape.radius)
			ids = g.rangeIDs(minLon, minLat, maxLon, maxLat)
			for id := range ids {
				p := g.points[id]
				if haversineMeters(shape.lon, shape.lat, p[0], p[1]) > shape.radius {
					delete(ids, id)
				}
			}
		}
		best = intersectIDs(best, ids)
		if len(best) == 0 {
			return best
		}
	}
	return best
}

// rangeIDs 在按经度排序的条目上二分出经度区间，再按纬度过滤。
func (g *geoIndex) rangeIDs(minLon, minLat, maxLon, maxLat float64) map[string]struct{} {
	lo := sort.Search(len(g.entries), func(i int) bool { return g.entries[i].lon >= minLon })
	hi := sort.Search(len(g.entries), func(i int) bool { return g.entries[i].lon > maxLon })
	out := make(map[string]struct{})
	for i := lo; i < hi; i++ {
		e := g.entries[i]
		if e.lat < minLat || e.lat > maxLat {
			continue
		}
		out[e.id] = struct{}{}
	}
	return out
}

func buildGeoIndex(c *collection, field string) *geoIndex {
	g := newGeoIndex(field)
	for _, id := range c.ids {
		if doc := c.docs[id]; doc != nil {
			g.add(id, doc)
		}
	}
	return g
}

// geoShape 是解析后的 $geoWithin 条件。
type geoShape struct {
	isBox                          bool
	minLon, minLat, maxLon, maxLat float64
	lon, lat, radius               float64
}

func parseGeoWithin(spec any) (geoShape, bool) {
	m, ok := spec.(map[string]any)
	if !ok {
		return geoShape{}, false
	}
	if box, has := m["$box"]; has && len(m) == 1 {
		items, ok := asArray(box)
		if !ok || len(items) != 2 {
			return geoShape{}, false
		}
		loLon, loLat, ok := geoPoint(items[0])
		if !ok {
			return geoShape{}, false
		}
		hiLon, hiLat, ok := geoPoint(items[1])
		if !ok {
			return geoShape{}, false
		}
		shape := geoShape{
			isBox:  true,
			minLon: math.Min(loLon, hiLon),
			minLat: math.Min(loLat, hiLat),
			maxLon: math.Max(loLon, hiLon),
			maxLat: math.Max(loLat, hiLat),
		}
		return shape, true
	}
	if circle, has := m["$circle"]; has && len(m) == 1 {
		cm, ok := circle.(map[string]any)
		if !ok {
			return geoShape{}, false
		}
		centerLon, centerLat, ok := geoPoint(cm["$center"])
		if !ok {
			return geoShape{}, false
		}
		radius, ok := toFloat(cm["$radius"])
		if !ok || radius < 0 {
			return geoShape{}, false
		}
		return geoShape{lon: centerLon, lat: centerLat, radius: radius}, true
	}
	return geoShape{}, false
}

// matchGeoWithin 判断文档中的点是否落在 $box 或 $circle 范围内。
func matchGeoWithin(actual any, exists bool, spec any) bool {
	if !exists {
		return false
	}
	lon, lat, ok := geoPoint(actual)
	if !ok {
		return false
	}
	shape, ok := parseGeoWithin(spec)
	if !ok {
		return false
	}
	if shape.isBox {
		return lon >= shape.minLon && lon <= shape.maxLon && lat >= shape.minLat && lat <= shape.maxLat
	}
	return haversineMeters(shape.lon, shape.lat, lon, lat) <= shape.radius
}

// geoPoint 从 [lon,lat] 或 GeoJSON Point 中解析坐标。
func geoPoint(v any) (lon, lat float64, ok bool) {
	if arr, isArr := asArray(v); isArr {
		if len(arr) < 2 {
			return 0, 0, false
		}
		x, ok1 := toFloat(arr[0])
		y, ok2 := toFloat(arr[1])
		if !ok1 || !ok2 || !validLonLat(x, y) {
			return 0, 0, false
		}
		return x, y, true
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		return 0, 0, false
	}
	if typ, has := m["type"].(string); has && !strings.EqualFold(typ, "Point") {
		return 0, 0, false
	}
	return geoPoint(m["coordinates"])
}

func validLonLat(lon, lat float64) bool {
	return lon >= -180 && lon <= 180 && lat >= -90 && lat <= 90
}

// circleBounds 把米制半径换算成经纬度包围盒，用于索引粗筛。
func circleBounds(lon, lat, radius float64) (minLon, minLat, maxLon, maxLat float64) {
	latDelta := radius / earthRadiusMeters * 180 / math.Pi
	minLat = math.Max(-90, lat-latDelta)
	maxLat = math.Min(90, lat+latDelta)
	cosLat := math.Cos(lat * math.Pi / 180)
	var lonDelta float64
	if math.Abs(cosLat) < 1e-9 {
		lonDelta = 180
	} else {
		lonDelta = latDelta / math.Abs(cosLat)
	}
	if lonDelta >= 180 {
		return -180, minLat, 180, maxLat
	}
	minLon = math.Max(-180, lon-lonDelta)
	maxLon = math.Min(180, lon+lonDelta)
	return minLon, minLat, maxLon, maxLat
}

// haversineMeters 计算两点间的球面距离（米）。
func haversineMeters(lon1, lat1, lon2, lat2 float64) float64 {
	const rad = math.Pi / 180
	phi1, phi2 := lat1*rad, lat2*rad
	dPhi := (lat2 - lat1) * rad
	dLambda := (lon2 - lon1) * rad
	a := math.Sin(dPhi/2)*math.Sin(dPhi/2) +
		math.Cos(phi1)*math.Cos(phi2)*math.Sin(dLambda/2)*math.Sin(dLambda/2)
	return 2 * earthRadiusMeters * math.Asin(math.Sqrt(a))
}
