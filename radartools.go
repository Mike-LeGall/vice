// radartools.go
// Copyright(c) 2022 Matt Pharr, licensed under the GNU Public License, Version 3.
// SPDX: GPL-3.0-only

package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mmp/imgui-go/v4"
	"github.com/nfnt/resize"
)

///////////////////////////////////////////////////////////////////////////
// WeatherRadar

// WeatherRadar provides functionality for fetching radar images to display
// in radar scopes. Only locations in the USA are currently supported, as
// the only current data source is the US NOAA. (TODO: find more sources
// and add support for them!)
type WeatherRadar struct {
	active bool

	// Images are fetched in a separate goroutine; updated radar center
	// locations are sent from the main thread via reqChan and downloaded
	// radar images are returned via imageChan.
	reqChan   chan Point2LL
	imageChan chan ImageAndBounds

	// radarBounds records the lat-long bounding box of the most recently
	// received radar image, which has texId as its GPU texture it.
	radarBounds Extent2D
	texId       uint32

	lastDraw time.Time

	// BlendFactor controls the blending of the radar image; 0 hides it and 1
	// shows it as received from the sender (which is normally far too bright
	// and obscures other things on the scope). Values around 0.1 or 0.2 are
	// generally reasonable.
	BlendFactor float32
}

// Latitude-longitude extent of the fetched image; the requests are +/-
// this much from the current center.
const weatherLatLongExtent = 5

type ImageAndBounds struct {
	img    image.Image
	bounds Extent2D
}

// Activate must be called for the WeatherRadar to start fetching weather
// radar images; it is called with an initial center position in
// latitude-longitude coordinates.
func (w *WeatherRadar) Activate(center Point2LL) {
	if w.active {
		lg.Errorf("Called Activate on already-active WeatherRadar")
		return
	}
	w.active = true

	w.reqChan = make(chan Point2LL, 1000) // lots of buffering
	w.reqChan <- center
	w.imageChan = make(chan ImageAndBounds) // unbuffered channel

	// NOAA posts new maps every 2 minutes, so fetch a new map at minimum
	// every 100s to stay current.
	go fetchWeather(w.reqChan, w.imageChan, 100*time.Second)
}

// Deactivate causes the WeatherRadar to stop fetching weather updates;
// it is important that this method be called when a radar scope is
// deactivated so that we don't continue to consume bandwidth fetching
// unneeded weather images.
func (w *WeatherRadar) Deactivate() {
	close(w.reqChan)
	w.active = false
}

// UpdateCenter provides a new center point for the radar image, causing a
// new image to be fetched.
func (w *WeatherRadar) UpdateCenter(center Point2LL) {
	select {
	case w.reqChan <- center:
		// success
	default:
		// The channel is full; this may happen if the user is continuously
		// dragging the radar scope around. Worst case, we drop some
		// position update requests, which is generally no big deal.
	}
}

func (w *WeatherRadar) DrawUI() {
	imgui.SliderFloatV("Weather radar blending factor", &w.BlendFactor, 0, 1, "%.2f", 0)
}

// fetchWeather runs asynchronously in a goroutine, receiving requests from
// reqChan, fetching corresponding radar images from the NOAA, and sending
// the results back on imageChan.  New images are also automatically
// fetched periodically, with a wait time specified by the delay parameter.
func fetchWeather(reqChan chan Point2LL, imageChan chan ImageAndBounds, delay time.Duration) {
	// center stores the current center position of the radar image
	var center Point2LL
	for {
		var ok, timedOut bool
		select {
		case center, ok = <-reqChan:
			if ok {
				// Drain any additional requests so that we get the most
				// recent one.
				for len(reqChan) > 0 {
					center = <-reqChan
				}
			} else {
				// The channel is closed; wrap up.
				close(imageChan)
				return
			}
		case <-time.After(delay):
			// Periodically make a new request even if the center hasn't
			// changed.
			timedOut = true
		}

		// Lat-long bounds of the region we're going to request weater for.
		rb := Extent2D{p0: sub2ll(center, Point2LL{weatherLatLongExtent, weatherLatLongExtent}),
			p1: add2ll(center, Point2LL{weatherLatLongExtent, weatherLatLongExtent})}

		// The weather radar image comes via a WMS GetMap request from the NOAA.
		//
		// Relevant background:
		// https://enterprise.arcgis.com/en/server/10.3/publish-services/windows/communicating-with-a-wms-service-in-a-web-browser.htm
		// http://schemas.opengis.net/wms/1.3.0/capabilities_1_3_0.xsd
		// NOAA weather: https://opengeo.ncep.noaa.gov/geoserver/www/index.html
		// https://opengeo.ncep.noaa.gov/geoserver/conus/conus_bref_qcd/ows?service=wms&version=1.3.0&request=GetCapabilities
		params := url.Values{}
		params.Add("SERVICE", "WMS")
		params.Add("REQUEST", "GetMap")
		params.Add("FORMAT", "image/png")
		params.Add("WIDTH", "1024")
		params.Add("HEIGHT", "1024")
		params.Add("LAYERS", "conus_bref_qcd")
		params.Add("BBOX", fmt.Sprintf("%f,%f,%f,%f", rb.p0[0], rb.p0[1], rb.p1[0], rb.p1[1]))

		url := "https://opengeo.ncep.noaa.gov/geoserver/conus/conus_bref_qcd/ows?" + params.Encode()
		lg.Printf("Fetching weather: %s", url)

		// Request the image
		resp, err := http.Get(url)
		if err != nil {
			lg.Printf("Weather error: %s", err)
			continue
		}
		defer resp.Body.Close()

		img, err := png.Decode(resp.Body)
		if err != nil {
			lg.Printf("Weather error: %s", err)
			continue
		}

		// Convert the Image returned by png.Decode to an RGBA image so
		// that we can patch up some of the pixel values.
		rgba := image.NewRGBA(img.Bounds())
		draw.Draw(rgba, img.Bounds(), img, image.Point{}, draw.Over)
		ny, nx := img.Bounds().Dy(), img.Bounds().Dx()
		for y := 0; y < ny; y++ {
			for x := 0; x < nx; x++ {
				r, g, b, a := img.At(x, y).RGBA()
				// Convert all-white to black and an alpha channel of zero, so
				// that where there's no weather, nothing is drawn.
				if r == 0xffff && g == 0xffff && b == 0xffff && a == 0xffff {
					rgba.Set(x, y, color.RGBA{})
				}
			}
		}

		// The image we get back is relatively low resolution (and doesn't
		// even have 1024x1024 pixels of actual detail); use a decent
		// filter to upsample it, which looks better than relying on GPU
		// bilinear interpolation...
		resized := resize.Resize(2048, 2048, rgba, resize.MitchellNetravali)

		// Send it back to the main thread.
		imageChan <- ImageAndBounds{img: resized, bounds: rb}
		lg.Printf("finish weather fetch")

		if !timedOut {
			time.Sleep(15 * time.Second)
		}
	}
}

// Draw draws the current weather radar image, if available. (If none is yet
// available, it returns rather than stalling waiting for it). The provided
// CommandBuffer should be set up with viewing matrices such that vertex
// coordinates are provided in latitude-longitude.
func (w *WeatherRadar) Draw(cb *CommandBuffer) {
	// Try to receive an updated image from the fetchWather goroutine, if
	// one is available.
	select {
	case ib, ok := <-w.imageChan:
		if ok {
			w.radarBounds = ib.bounds
			if w.texId == 0 {
				w.texId = renderer.CreateTextureFromImage(ib.img, false)
			} else {
				renderer.UpdateTextureFromImage(w.texId, ib.img, false)
			}
		}
	default:
		// no message
	}

	// Note that we always go ahead and drain the imageChan, even if if the
	// WeatherRadar is inactive. This way the chan is ready for the
	// future...
	if !w.active {
		return
	}

	if w.texId == 0 {
		// Presumably we haven't yet gotten a response to the initial
		// request...
		return
	}

	// We have a valid radar image, so draw it.
	cb.SetRGBA(RGBA{1, 1, 1, w.BlendFactor})
	cb.Blend()
	cb.EnableTexture(w.texId)

	// Draw the lat-long space quad corresponding to the region that we
	// have weather for; just stuff the vertex and index buffers into the
	// CommandBuffer directly rather than bothering with a
	// TrianglesDrawable or the like.
	rb := w.radarBounds
	p := [4][2]float32{[2]float32{rb.p0[0], rb.p0[1]}, [2]float32{rb.p1[0], rb.p0[1]},
		[2]float32{rb.p1[0], rb.p1[1]}, [2]float32{rb.p0[0], rb.p1[1]}}
	pidx := cb.Float2Buffer(p[:])
	cb.VertexArray(pidx, 2, 2*4)

	uv := [4][2]float32{[2]float32{0, 1}, [2]float32{1, 1}, [2]float32{1, 0}, [2]float32{0, 0}}
	uvidx := cb.Float2Buffer(uv[:])
	cb.TexCoordArray(uvidx, 2, 2*4)

	indidx := cb.IntBuffer([]int32{0, 1, 2, 3})
	cb.DrawQuads(indidx, 4)

	cb.DisableTexture()
	cb.DisableBlend()
}

///////////////////////////////////////////////////////////////////////////
// CRDA

type CRDAConfig struct {
	Airport                  string
	PrimaryRunway            string
	SecondaryRunway          string
	Mode                     int
	TieStaggerDistance       float32
	ShowGhostsOnPrimary      bool
	HeadingTolerance         float32
	GlideslopeLateralSpread  float32
	GlideslopeVerticalSpread float32
	GlideslopeAngle          float32
	ShowCRDARegions          bool
}

const (
	CRDAModeStagger = iota
	CRDAModeTie
)

func NewCRDAConfig() CRDAConfig {
	return CRDAConfig{
		Mode:                     CRDAModeStagger,
		TieStaggerDistance:       3,
		HeadingTolerance:         110,
		GlideslopeLateralSpread:  10,
		GlideslopeVerticalSpread: 10,
		GlideslopeAngle:          3}

}

func (c *CRDAConfig) getRunway(n string) *Runway {
	for _, rwy := range database.runways[c.Airport] {
		if rwy.number == n {
			return &rwy
		}
	}
	return nil
}

func (c *CRDAConfig) getRunways() (ghostSource *Runway, ghostDestination *Runway) {
	for i, rwy := range database.runways[c.Airport] {
		if rwy.number == c.PrimaryRunway {
			ghostSource = &database.runways[c.Airport][i]
		}
		if rwy.number == c.SecondaryRunway {
			ghostDestination = &database.runways[c.Airport][i]
		}
	}

	if c.ShowGhostsOnPrimary {
		ghostSource, ghostDestination = ghostDestination, ghostSource
	}

	return
}

func runwayIntersection(a *Runway, b *Runway) (Point2LL, bool) {
	p1, p2 := ll2nm(a.threshold), ll2nm(a.end)
	p3, p4 := ll2nm(b.threshold), ll2nm(b.end)
	p, ok := LineLineIntersect(p1, p2, p3, p4)

	centroid := mid2f(mid2f(p1, p2), mid2f(p3, p4))
	d := distance2f(centroid, p)
	if d > 30 {
		// more like parallel; we don't care about super far away intersections...
		ok = false
	}

	return nm2ll(p), ok
}

func (c *CRDAConfig) GetGhost(ac *Aircraft) *Aircraft {
	src, dst := c.getRunways()
	if src == nil || dst == nil {
		return nil
	}

	pIntersect, ok := runwayIntersection(src, dst)
	if !ok {
		lg.Printf("No intersection between runways??!?")
		return nil
	}

	airport, ok := database.FAA.airports[c.Airport]
	if !ok {
		lg.Printf("%s: airport unknown?!", c.Airport)
		return nil
	}

	if ac.GroundSpeed() > 350 {
		return nil
	}

	if headingDifference(ac.Heading(), src.heading) > c.HeadingTolerance {
		return nil
	}

	// Is it on the glideslope?
	// Laterally: compute the heading to the threshold and compare to the
	// glideslope's lateral spread.
	h := headingp2ll(ac.Position(), src.threshold, database.MagneticVariation)
	if fabs(h-src.heading) > c.GlideslopeLateralSpread {
		return nil
	}

	// Vertically: figure out the range of altitudes at the distance out.
	// First figure out the aircraft's height AGL.
	agl := ac.Altitude() - airport.elevation

	// Find the glideslope height at the aircraft's distance to the
	// threshold.
	// tan(glideslope angle) = height / threshold distance
	const nmToFeet = 6076.12
	thresholdDistance := nmToFeet * nmdistance2ll(ac.Position(), src.threshold)
	height := thresholdDistance * tan(radians(c.GlideslopeAngle))
	// Assume 100 feet at the threshold
	height += 100

	// Similarly, find the allowed altitude difference
	delta := thresholdDistance * tan(radians(c.GlideslopeVerticalSpread))

	if fabs(float32(agl)-height) > delta {
		return nil
	}

	// This aircraft gets a ghost.

	// This is a little wasteful, but we're going to copy the entire
	// Aircraft structure just to be sure we carry along everything we
	// might want to have available when drawing the track and
	// datablock for the ghost.
	ghost := *ac

	// Now we just need to update the track positions to be those for
	// the ghost. We'll again do this in nm space before going to
	// lat-long in the end.
	pi := ll2nm(pIntersect)
	for i, t := range ghost.tracks {
		// Vector from the intersection point to the track location
		v := sub2f(ll2nm(t.position), pi)

		// For tie mode, offset further by the specified distance.
		if c.Mode == CRDAModeTie {
			length := length2f(v)
			v = scale2f(v, (length+c.TieStaggerDistance)/length)
		}

		// Rotate it angle degrees clockwise
		angle := dst.heading - src.heading
		s, c := sin(radians(angle)), cos(radians(angle))
		vr := [2]float32{c*v[0] + s*v[1], -s*v[0] + c*v[1]}
		// Point along the other runway
		pr := add2f(pi, vr)

		// TODO: offset it as appropriate
		ghost.tracks[i].position = nm2ll(pr)
	}
	return &ghost
}

func (c *CRDAConfig) DrawUI() bool {
	updateGhosts := false

	flags := imgui.InputTextFlagsCharsUppercase | imgui.InputTextFlagsCharsNoBlank
	imgui.InputTextV("Airport", &c.Airport, flags, nil)
	if runways, ok := database.runways[c.Airport]; !ok {
		if c.Airport != "" {
			color := positionConfig.GetColorScheme().TextError
			imgui.PushStyleColor(imgui.StyleColorText, color.imgui())
			imgui.Text("Airport unknown!")
			imgui.PopStyleColor()
		}
	} else {
		sort.Slice(runways, func(i, j int) bool { return runways[i].number < runways[j].number })

		primary, secondary := c.getRunway(c.PrimaryRunway), c.getRunway(c.SecondaryRunway)
		if imgui.BeginComboV("Primary runway", c.PrimaryRunway, imgui.ComboFlagsHeightLarge) {
			if imgui.SelectableV("(None)", c.PrimaryRunway == "", 0, imgui.Vec2{}) {
				updateGhosts = true
				c.PrimaryRunway = ""
			}
			for _, rwy := range runways {
				if secondary != nil {
					// Don't include the selected secondary runway
					if rwy.number == secondary.number {
						continue
					}
					// Only list intersecting runways
					if _, ok := runwayIntersection(&rwy, secondary); !ok {
						continue
					}
				}
				if imgui.SelectableV(rwy.number, rwy.number == c.PrimaryRunway, 0, imgui.Vec2{}) {
					updateGhosts = true
					c.PrimaryRunway = rwy.number
				}
			}
			imgui.EndCombo()
		}
		if imgui.BeginComboV("Secondary runway", c.SecondaryRunway, imgui.ComboFlagsHeightLarge) {
			// Note: this is the exact same logic for primary runways
			// above, just with the roles switched...
			if imgui.SelectableV("(None)", c.SecondaryRunway == "", 0, imgui.Vec2{}) {
				updateGhosts = true
				c.SecondaryRunway = ""
			}
			for _, rwy := range runways {
				if primary != nil {
					// Don't include the selected primary runway
					if rwy.number == primary.number {
						continue
					}
					// Only list intersecting runways
					if _, ok := runwayIntersection(&rwy, primary); !ok {
						continue
					}
				}
				if imgui.SelectableV(rwy.number, rwy.number == c.SecondaryRunway, 0, imgui.Vec2{}) {
					updateGhosts = true
					c.SecondaryRunway = rwy.number
				}
			}
			imgui.EndCombo()
		}
		if imgui.Checkbox("Ghosts on primary", &c.ShowGhostsOnPrimary) {
			updateGhosts = true
		}
		imgui.Text("Mode")
		imgui.SameLine()
		updateGhosts = imgui.RadioButtonInt("Stagger", &c.Mode, 0) || updateGhosts
		imgui.SameLine()
		updateGhosts = imgui.RadioButtonInt("Tie", &c.Mode, 1) || updateGhosts
		if c.Mode == CRDAModeTie {
			imgui.SameLine()
			updateGhosts = imgui.SliderFloatV("Tie stagger distance", &c.TieStaggerDistance, 0.1, 10, "%.1f", 0) ||
				updateGhosts
		}
		updateGhosts = imgui.SliderFloatV("Heading tolerance (deg)", &c.HeadingTolerance, 5, 180, "%.0f", 0) || updateGhosts
		updateGhosts = imgui.SliderFloatV("Glideslope angle (deg)", &c.GlideslopeAngle, 2, 5, "%.1f", 0) || updateGhosts
		updateGhosts = imgui.SliderFloatV("Glideslope lateral spread (deg)", &c.GlideslopeLateralSpread, 1, 20, "%.0f", 0) || updateGhosts
		updateGhosts = imgui.SliderFloatV("Glideslope vertical spread (deg)", &c.GlideslopeVerticalSpread, 1, 10, "%.1f", 0) || updateGhosts
		updateGhosts = imgui.Checkbox("Show CRDA regions", &c.ShowCRDARegions) || updateGhosts
	}

	return updateGhosts
}

func (rs *RadarScopePane) drawCRDARegions(ctx *PaneContext) {
	if !rs.CRDAConfig.ShowCRDARegions {
		return
	}

	// Find the intersection of the two runways.  Work in nm space, not lat-long
	if true {
		src, dst := rs.CRDAConfig.getRunways()
		if src != nil && dst != nil {
			p, ok := runwayIntersection(src, dst)
			if !ok {
				lg.Printf("no intersection between runways?!")
			}
			//		rs.linesDrawBuilder.AddLine(src.threshold, src.end, RGB{0, 1, 0})
			//		rs.linesDrawBuilder.AddLine(dst.threshold, dst.end, RGB{0, 1, 0})
			rs.pointsDrawBuilder.AddPoint(p, RGB{1, 0, 0})
		}
	}

	src, _ := rs.CRDAConfig.getRunways()
	if src == nil {
		return
	}

	// we have the runway heading, but we want to go the opposite direction
	// and then +/- HeadingTolerance.
	rota := src.heading + 180 - rs.CRDAConfig.GlideslopeLateralSpread - database.MagneticVariation
	rotb := src.heading + 180 + rs.CRDAConfig.GlideslopeLateralSpread - database.MagneticVariation

	// Lay out the vectors in nm space, not lat-long
	sa, ca := sin(radians(rota)), cos(radians(rota))
	va := [2]float32{sa, ca}
	dist := float32(25)
	va = scale2f(va, dist)

	sb, cb := sin(radians(rotb)), cos(radians(rotb))
	vb := scale2f([2]float32{sb, cb}, dist)

	// Over to lat-long to draw the lines
	vall, vbll := nm2ll(va), nm2ll(vb)
	rs.linesDrawBuilder.AddLine(src.threshold, add2ll(src.threshold, vall), ctx.cs.Caution)
	rs.linesDrawBuilder.AddLine(src.threshold, add2ll(src.threshold, vbll), ctx.cs.Caution)
}

///////////////////////////////////////////////////////////////////////////
// DataBlockFormat

// Loosely patterened after https://vrc.rosscarlson.dev/docs/single_page.html#the_various_radar_modes
const (
	DataBlockFormatNone = iota
	DataBlockFormatSimple
	DataBlockFormatGround
	DataBlockFormatTower
	DataBlockFormatFull
	DataBlockFormatCount
)

type DataBlockFormat int

func (d DataBlockFormat) String() string {
	return [...]string{"None", "Simple", "Ground", "Tower", "Full"}[d]
}

func (d *DataBlockFormat) DrawUI() bool {
	changed := false
	if imgui.BeginCombo("Data block format", d.String()) {
		var i DataBlockFormat
		for ; i < DataBlockFormatCount; i++ {
			if imgui.SelectableV(DataBlockFormat(i).String(), i == *d, 0, imgui.Vec2{}) {
				*d = i
				changed = true
			}
		}
		imgui.EndCombo()
	}
	return changed
}

func (d DataBlockFormat) Format(ac *Aircraft, duplicateSquawk bool, flashcycle int) string {
	if d == DataBlockFormatNone {
		return ""
	}

	alt100s := (ac.Altitude() + 50) / 100
	speed := ac.GroundSpeed()
	fp := ac.flightPlan

	if fp == nil {
		return ac.squawk.String() + fmt.Sprintf(" %03d", alt100s)
	}

	actype := fp.TypeWithoutSuffix()
	if actype != "" {
		// So we can unconditionally print it..
		actype += " "
	}

	var datablock strings.Builder
	datablock.Grow(64)

	// All of the modes always start with the callsign and the voicce indicator
	datablock.WriteString(ac.Callsign())
	// Otherwise a 3 line datablock
	// Line 1: callsign and voice indicator
	if ac.voiceCapability == VoiceReceive {
		datablock.WriteString("/r")
	} else if ac.voiceCapability == VoiceText {
		datablock.WriteString("/t")
	}

	switch d {
	case DataBlockFormatSimple:
		return datablock.String()

	case DataBlockFormatGround:
		datablock.WriteString("\n")
		// Line 2: a/c type and groundspeed
		datablock.WriteString(actype)

		// normally it's groundspeed next, unless there's a squawk
		// situation that we need to flag...
		if duplicateSquawk && ac.mode != Standby && ac.squawk != Squawk(1200) && ac.squawk != 0 && flashcycle&1 == 0 {
			datablock.WriteString("CODE")
		} else if !duplicateSquawk && ac.mode != Standby && ac.squawk != ac.assignedSquawk && flashcycle&1 == 0 {
			datablock.WriteString(ac.squawk.String())
		} else {
			datablock.WriteString(fmt.Sprintf("%02d", speed))
			if fp.rules == VFR {
				datablock.WriteString("V")
			}
		}
		return datablock.String()

	case DataBlockFormatTower:
		// Line 2: first flash is [alt speed/10]. If we don't have
		// destination and a/c type then just always show this rather than
		// flashing a blank line.
		datablock.WriteString("\n")
		if flashcycle&1 == 0 || (fp.arrive == "" && actype == "") {
			datablock.WriteString(fmt.Sprintf("%03d %02d", alt100s, (speed+5)/10))
			if fp.rules == VFR {
				datablock.WriteString("V")
			}
		} else {
			// Second flash normally alternates between scratchpad (or dest) and
			// filed altitude for the first thing, then has *[actype]
			if flashcycle&2 == 0 {
				if ac.scratchpad != "" {
					datablock.WriteString(ac.scratchpad)
				} else {
					datablock.WriteString(fp.arrive)
				}
			} else {
				// Second field is the altitude
				datablock.WriteString(fmt.Sprintf("%03d", fp.altitude/100))
			}

			datablock.WriteString("*")
			// Flag squawk issues
			if duplicateSquawk && ac.mode != Standby && ac.squawk != 0 && flashcycle&1 == 0 {
				datablock.WriteString("CODE")
			} else if !duplicateSquawk && ac.mode != Standby && ac.squawk != ac.assignedSquawk && flashcycle&1 == 0 {
				datablock.WriteString(ac.squawk.String())
			} else {
				datablock.WriteString(actype)
			}
		}
		return datablock.String()

	case DataBlockFormatFull:
		if ac.mode == Standby {
			return datablock.String()
		}

		dalt := ac.AltitudeChange()
		ascending, descending := dalt > 250, dalt < -250
		altAnnotation := " "
		if ac.tempAltitude != 0 && abs(ac.Altitude()-ac.tempAltitude) < 300 {
			altAnnotation = "T "
		} else if ac.flightPlan.altitude != 0 &&
			abs(ac.Altitude()-ac.flightPlan.altitude) < 300 {
			altAnnotation = "C "
		} else if ascending {
			altAnnotation = FontAwesomeIconArrowUp + " "
		} else if descending {
			altAnnotation = FontAwesomeIconArrowDown + " "
		}

		if ac.squawk == Squawk(1200) {
			// VFR
			datablock.WriteString(fmt.Sprintf(" %03d", alt100s))
			datablock.WriteString(altAnnotation)
			return datablock.String()
		}
		datablock.WriteString("\n")

		// Line 2: altitude, then scratchpad or temp/assigned altitude.
		datablock.WriteString(fmt.Sprintf("%03d", alt100s))
		datablock.WriteString(altAnnotation)
		// TODO: Here add level if at wrong alt...

		// Have already established it's not squawking standby.
		if duplicateSquawk && ac.squawk != Squawk(1200) && ac.squawk != 0 {
			if flashcycle&1 == 0 {
				datablock.WriteString("CODE")
			} else {
				datablock.WriteString(ac.squawk.String())
			}
		} else if ac.squawk != ac.assignedSquawk {
			// show what they are actually squawking
			datablock.WriteString(ac.squawk.String())
		} else {
			if flashcycle&1 == 0 {
				if ac.scratchpad != "" {
					datablock.WriteString(ac.scratchpad)
				} else if ac.tempAltitude != 0 {
					datablock.WriteString(fmt.Sprintf("%03dT", ac.tempAltitude/100))
				} else {
					datablock.WriteString(fmt.Sprintf("%03d", fp.altitude/100))
				}
			} else {
				if fp.arrive != "" {
					datablock.WriteString(fp.arrive)
				} else {
					datablock.WriteString("????")
				}
			}
		}
		datablock.WriteString("\n")

		// Line 3: a/c type and groundspeed
		datablock.WriteString(actype)
		datablock.WriteString(fmt.Sprintf("%03d", (speed+5)/10*10))
		if fp.rules == VFR {
			datablock.WriteString("V")
		}

		if ac.mode == Ident && flashcycle&1 == 0 {
			datablock.WriteString("ID")
		}

		return datablock.String()

	default:
		lg.Printf("%d: unhandled datablock format", d)
		return "ERROR"
	}
}