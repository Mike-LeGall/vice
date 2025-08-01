// pkg/panes/stars/stars.go
// Copyright(c) 2022-2024 vice contributors, licensed under the GNU Public License, Version 3.
// SPDX: GPL-3.0-only

package stars

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	av "github.com/mmp/vice/pkg/aviation"
	"github.com/mmp/vice/pkg/client"
	"github.com/mmp/vice/pkg/log"
	"github.com/mmp/vice/pkg/math"
	"github.com/mmp/vice/pkg/panes"
	"github.com/mmp/vice/pkg/platform"
	"github.com/mmp/vice/pkg/radar"
	"github.com/mmp/vice/pkg/renderer"
	"github.com/mmp/vice/pkg/sim"
	"github.com/mmp/vice/pkg/util"

	"github.com/AllenDang/cimgui-go/imgui"
)

// IFR TRACON separation requirements
const LateralMinimum = 3
const VerticalMinimum = 1000

// STARS ∆ is character 0x80 in the font
const STARSTriangleCharacter = string(rune(0x80))

// Filled upward-pointing triangle
const STARSFilledUpTriangle = string(rune(0x1e))

const TabListEntries = 100
const TabListUnassignedIndex = -1

var (
	STARSBackgroundColor    = renderer.RGB{.2, .2, .2} // at 100 contrast
	STARSListColor          = renderer.RGB{.1, .9, .1}
	STARSTextAlertColor     = renderer.RGB{1, 0, 0}
	STARSTextWarningColor   = renderer.RGB{1, 1, 0}
	STARSCompassColor       = renderer.RGB{.55, .55, .55}
	STARSRangeRingColor     = renderer.RGB{.55, .55, .55}
	STARSTrackBlockColor    = renderer.RGB{0.12, 0.48, 1}
	STARSTrackHistoryColors = [5]renderer.RGB{
		renderer.RGB{.12, .31, .78},
		renderer.RGB{.28, .28, .67},
		renderer.RGB{.2, .2, .51},
		renderer.RGB{.16, .16, .43},
		renderer.RGB{.12, .12, .35},
	}
	STARSJRingConeColor         = renderer.RGB{.5, .5, 1}
	STARSTrackedAircraftColor   = renderer.RGB{1, 1, 1}
	STARSUntrackedAircraftColor = renderer.RGB{0, 1, 0}
	STARSTrackAlertColor        = renderer.RGB{1, 1, 0}
	STARSGhostColor             = renderer.RGB{1, 1, 0}
	STARSSelectedAircraftColor  = renderer.RGB{0, 1, 1}

	STARSATPAWarningColor = renderer.RGB{1, 1, 0}
	STARSATPAAlertColor   = renderer.RGB{1, .215, 0}
)

type STARSPane struct {
	TRACONPreferenceSets map[string]*PreferenceSet
	prefSet              *PreferenceSet

	// These are the current prefs from the prior representation; we read
	// them back in if they're there to use to bootstrap the new
	// representation.
	// TODO: remove this at some point in the future.
	OldPrefsCurrentPreferenceSet  *Preferences  `json:"CurrentPreferenceSet,omitempty"`
	OldPrefsSelectedPreferenceSet *int          `json:"SelectedPreferenceSet,omitempty"`
	OldPrefsPreferenceSets        []Preferences `json:"PreferenceSets,omitempty"`

	allVideoMaps []sim.VideoMap
	dcbVideoMaps []*sim.VideoMap

	weatherRadar radar.WeatherRadar

	targetGenLastCallsign av.ADSBCallsign

	// Which weather history snapshot to draw: this is always 0 unless the
	// 'display weather history' command was entered.
	wxHistoryDraw int
	// Time at which to step to the next history snapshot (5s intervals).
	wxNextHistoryStepTime time.Time

	systemFontA, systemFontB               [6]*renderer.Font
	systemOutlineFontA, systemOutlineFontB [6]*renderer.Font
	dcbFontA, dcbFontB                     [3]*renderer.Font // 0, 1, 2 only
	cursorsFont                            *renderer.Font
	hideMouseCursor                        bool // set after auto-home and the cursor is repositioned

	fusedTrackVertices [][2]float32

	events *sim.EventsSubscription

	// Preferences that were active when we entered the PREF menu.
	RestorePreferences       *Preferences
	RestorePreferencesNumber *int

	// It seems like this should be based on ACID but then we also need
	// state for unassociated tracks, so... ?
	TrackState map[av.ADSBCallsign]*TrackState

	LockDisplay bool

	// a/c callsign -> controllers
	PointOuts         map[sim.ACID]PointOutControllers
	RejectedPointOuts map[sim.ACID]interface{}
	ForceQLACIDs      map[sim.ACID]interface{}

	CoastSuspendIndex int // Next index to assign

	// Hold for release callsigns we have seen but not released. (We need
	// to track this since auto release only applies to new ones seen after
	// it is enabled.)
	ReleaseRequests map[av.ADSBCallsign]interface{}

	// Periodically updated in processEvents
	DuplicateBeacons map[av.Squawk]interface{}

	queryUnassociated *util.TransientMap[av.ADSBCallsign, interface{}]

	RangeBearingLines []STARSRangeBearingLine
	MinSepAircraft    [2]av.ADSBCallsign

	CAAircraft  []CAAircraft
	MCIAircraft []CAAircraft

	// For CRDA
	ConvergingRunways []STARSConvergingRunways

	// Various UI state
	FlipNumericKeypad bool
	TgtGenKey         byte

	FontSelection int32

	DisplayBeaconCode        av.Squawk
	DisplayBeaconCodeEndTime time.Time

	DisplayRequestedAltitude bool

	// When VFR flight plans were first seen (used for sorting in VFR list)
	VFRFPFirstSeen map[sim.ACID]time.Time

	scopeClickHandler scopeClickHandlerFunc
	activeSpinner     dcbSpinner

	savedMousePosition [2]float32
	accumMouseDeltaY   float32

	dwellAircraft     av.ADSBCallsign
	drawRouteAircraft av.ADSBCallsign

	drawRoutePoints []math.Point2LL

	commandMode       CommandMode
	multiFuncPrefix   string
	previewAreaOutput string
	previewAreaInput  string
	dcbShowAux        bool

	lastTrackUpdate        time.Time
	lastHistoryTrackUpdate time.Time
	discardTracks          bool

	// The start of a RBL--one click received, waiting for the second.
	wipRBL *STARSRangeBearingLine

	// First point clicked for display bearing/range to significant point.
	wipSignificantPoint *math.Point2LL

	audioEffects     map[AudioType]int // to handle from Platform.AddPCM()
	testAudioEndTime time.Time

	highlightedLocation        math.Point2LL
	highlightedLocationEndTime time.Time

	// Built-in screenshots / video captures
	capture struct {
		enabled          bool
		specifyingRegion bool
		haveRegion       bool
		region           [2][2]float32
		doStill          bool
		doVideo          bool
		video            struct {
			frameCh   chan *image.RGBA
			lastFrame time.Time
		}
	}

	// An in-progress restriction area.
	wipRestrictionArea           *av.RestrictionArea
	wipRestrictionAreaMousePos   [2]float32 // last click position while defining it
	wipRestrictionAreaMouseMoved bool       // has moved since last click

	// We won't waste the space to serialize these but reconstruct them on load.
	significantPoints map[string]sim.SignificantPoint
	// Store them redundantly in a slice so we can sort them and then
	// search in a consistent order (when we have to do an exhaustive
	// search).
	significantPointsSlice []sim.SignificantPoint

	showVFRAirports bool
	scopeDraw       struct {
		arrivals    map[string]map[int]bool               // group->index
		approaches  map[string]map[string]bool            // airport->approach
		departures  map[string]map[string]map[string]bool // airport->runway->exit
		overflights map[string]map[int]bool               // group->index
		airspace    map[string]map[string]bool            // ctrl -> volume name
	}

	// Instrument Flight Procedure (SIDs, STARs, IAPs etc) Helpers
	IFPHelpers struct {
		ArrivalsColor    *[3]float32
		ApproachesColor  *[3]float32
		DeparturesColor  *[3]float32
		OverflightsColor *[3]float32
		AirspaceColor    *[3]float32
	}

	windDrawAltitude int

	// We keep a pool of each type so that we don't need to allocate a new
	// object each time we generate a datablock.
	fdbArena util.ObjectArena[fullDatablock]
	pdbArena util.ObjectArena[partialDatablock]
	ldbArena util.ObjectArena[limitedDatablock]
	sdbArena util.ObjectArena[suspendedDatablock]
}

type scopeClickHandlerFunc func(*panes.Context, *STARSPane, []sim.Track, [2]float32, radar.ScopeTransformations) CommandStatus

type PointOutControllers struct {
	From, To string
}

const (
	fontDefault = iota
	fontLegacy
	fontARTS
)

func init() {
	panes.RegisterUnmarshalPane("STARSPane", func(d []byte) (panes.Pane, error) {
		var p STARSPane
		err := json.Unmarshal(d, &p)
		return &p, err
	})
}

type AudioType int

// The types of events we may play audio for.
const (
	AudioConflictAlert = iota
	AudioSquawkSPC
	AudioMinimumSafeAltitudeWarning
	AudioModeCIntruder
	AudioTest
	AudioInboundHandoff
	AudioCommandError
	AudioHandoffAccepted
	AudioNumTypes
)

func (ae AudioType) String() string {
	return [...]string{
		"Conflict Alert",
		"Emergency Squawk Code",
		"Minimum Safe Altitude Warning",
		"Mode C Intruder",
		"Test",
		"Inbound Handoff",
		"Command Error",
		"Handoff Accepted",
	}[ae]
}

// Used both for CAs and MCIs.
type CAAircraft struct {
	ADSBCallsigns [2]av.ADSBCallsign // sorted alphabetically
	Acknowledged  bool
	SoundEnd      time.Time
	Start         time.Time
}

type CRDAMode int

const (
	CRDAModeStagger = iota
	CRDAModeTie
)

// this is read-only, stored in STARSPane for convenience
type STARSConvergingRunways struct {
	av.ConvergingRunways
	ApproachRegions [2]*av.ApproachRegion
	Airport         string
	Index           int
}

type CRDARunwayState struct {
	Enabled                 bool
	LeaderLineDirection     *math.CardinalOrdinalDirection // nil -> unset
	DrawCourseLines         bool
	DrawQualificationRegion bool
}

// stores the per-preference set state for each STARSConvergingRunways
type CRDARunwayPairState struct {
	Enabled     bool
	Mode        CRDAMode
	RunwayState [2]CRDARunwayState
}

func (c *STARSConvergingRunways) getRunwaysString() string {
	return c.Runways[0] + "/" + c.Runways[1]
}

type VideoMapsGroup int

const (
	VideoMapNoCategory = iota - 1
	VideoMapGeographicMaps
	VideoMapControlledAirspace
	VideoMapRunwayExtensions
	VideoMapDangerAreas
	VideoMapAerodromes
	VideoMapGeneralAviation
	VideoMapSIDsSTARs
	VideoMapMilitary
	VideoMapGeographicPoints
	VideoMapProcessingAreas
	VideoMapCurrent
	VideoMapNumCategories
)

type DwellMode int

const (
	// Make 0 be "on" so zero-initialization gives "on"
	DwellModeOn = iota
	DwellModeLock
	DwellModeOff
)

func (d DwellMode) String() string {
	switch d {
	case DwellModeOn:
		return "ON"

	case DwellModeLock:
		return "LOCK"

	case DwellModeOff:
		return "OFF"

	default:
		return "unhandled DwellMode"
	}
}

type STARSBrightness int

func (b STARSBrightness) RGB() renderer.RGB {
	v := float32(b) / 100
	return renderer.RGB{v, v, v}
}

func (b STARSBrightness) ScaleRGB(r renderer.RGB) renderer.RGB {
	return r.Scale(float32(b) / 100)
}

///////////////////////////////////////////////////////////////////////////
// STARSPane proper

func NewSTARSPane() *STARSPane {
	return &STARSPane{}
}

func (sp *STARSPane) DisplayName() string { return "STARS" }

func (sp *STARSPane) Hide() bool { return false }

func (sp *STARSPane) Activate(r renderer.Renderer, p platform.Platform, eventStream *sim.EventStream, lg *log.Logger) {
	if sp.PointOuts == nil {
		sp.PointOuts = make(map[sim.ACID]PointOutControllers)
	}
	if sp.RejectedPointOuts == nil {
		sp.RejectedPointOuts = make(map[sim.ACID]interface{})
	}
	if sp.queryUnassociated == nil {
		sp.queryUnassociated = util.NewTransientMap[av.ADSBCallsign, interface{}]()
	}
	if sp.TRACONPreferenceSets == nil {
		sp.TRACONPreferenceSets = make(map[string]*PreferenceSet)
	}

	sp.initializeFonts(r, p)
	sp.initializeAudio(p, lg)

	if sp.TrackState == nil {
		sp.TrackState = make(map[av.ADSBCallsign]*TrackState)
	}
	if sp.VFRFPFirstSeen == nil {
		sp.VFRFPFirstSeen = make(map[sim.ACID]time.Time)
	}

	sp.events = eventStream.Subscribe()

	sp.weatherRadar.Activate(r, lg)

	sp.lastTrackUpdate = time.Time{} // force immediate update at start
	sp.lastHistoryTrackUpdate = time.Time{}

	if sp.TgtGenKey == 0 {
		sp.TgtGenKey = ';'
	}

	if sp.IFPHelpers.ApproachesColor == nil {
		sp.IFPHelpers.ApproachesColor = &[3]float32{.1, .9, .1}
	}

	if sp.IFPHelpers.ArrivalsColor == nil {
		sp.IFPHelpers.ArrivalsColor = &[3]float32{.1, .9, .1}
	}

	if sp.IFPHelpers.DeparturesColor == nil {
		sp.IFPHelpers.DeparturesColor = &[3]float32{.1, .9, .1}
	}

	if sp.IFPHelpers.OverflightsColor == nil {
		sp.IFPHelpers.OverflightsColor = &[3]float32{.1, .9, .1}
	}

	if sp.IFPHelpers.AirspaceColor == nil {
		sp.IFPHelpers.AirspaceColor = &[3]float32{.1, .9, .1}
	}

	sp.capture.enabled = os.Getenv("VICE_CAPTURE") != ""
}

func (sp *STARSPane) LoadedSim(client *client.ControlClient, ss sim.State, pl platform.Platform, lg *log.Logger) {
	sp.DisplayRequestedAltitude = client.State.STARSFacilityAdaptation.FDB.DisplayRequestedAltitude

	sp.initPrefsForLoadedSim(ss, pl)
	sp.weatherRadar.UpdateCenter(sp.currentPrefs().DefaultCenter)

	sp.makeMaps(client, ss, lg)
	sp.makeSignificantPoints(ss)
}

func (sp *STARSPane) ResetSim(client *client.ControlClient, ss sim.State, pl platform.Platform, lg *log.Logger) {
	sp.ConvergingRunways = nil
	for _, name := range util.SortedMapKeys(ss.Airports) {
		ap := ss.Airports[name]
		for idx, pair := range ap.ConvergingRunways {
			sp.ConvergingRunways = append(sp.ConvergingRunways, STARSConvergingRunways{
				ConvergingRunways: pair,
				ApproachRegions: [2]*av.ApproachRegion{ap.ApproachRegions[pair.Runways[0]],
					ap.ApproachRegions[pair.Runways[1]]},
				Airport: name[1:], // drop the leading "K"
				Index:   idx + 1,  // 1-based
			})
		}
	}
	clear(sp.VFRFPFirstSeen)

	// Update maps before resetting the prefs since we may rewrite some map
	// ids and we want to use the right ones when we're enabling the
	// default maps.
	sp.makeMaps(client, ss, lg)
	sp.makeSignificantPoints(ss)

	sp.resetPrefsForNewSim(ss, pl)

	sp.weatherRadar.UpdateCenter(sp.currentPrefs().DefaultCenter)

	sp.lastTrackUpdate = time.Time{} // force update
	sp.lastHistoryTrackUpdate = time.Time{}

	// nil these out rather than clearing them so that they are rebuilt
	// from scratch.
	sp.scopeDraw.arrivals = nil
	sp.scopeDraw.approaches = nil
	sp.scopeDraw.departures = nil
	sp.scopeDraw.overflights = nil
	sp.scopeDraw.airspace = nil
}

func (sp *STARSPane) makeMaps(client *client.ControlClient, ss sim.State, lg *log.Logger) {
	usedIds := make(map[int]interface{})

	addMap := func(vm sim.VideoMap) {
		for i := range 999 {
			// See if id is available
			id := (vm.Id + i) % 1000

			if _, ok := usedIds[id]; !ok {
				vm.Id = id
				sp.allVideoMaps = append(sp.allVideoMaps, vm)
				usedIds[id] = nil
				return
			}
		}
		// Unable to find a free slot!
	}

	vmf, err := sp.getVideoMapLibrary(ss, client)
	if err != nil {
		lg.Errorf("%v", err)
	}

	// First grab the video maps needed for the DCB
	sp.allVideoMaps = util.FilterSlice(vmf.Maps, func(vm sim.VideoMap) bool {
		return slices.Contains(ss.ControllerVideoMaps, vm.Name)
	})
	for _, vm := range sp.allVideoMaps {
		usedIds[vm.Id] = nil
	}

	// Now make a second pass through the maps and add all of the ones that
	// don't have a conflicting ID with an existing map.
	for _, vm := range vmf.Maps {
		if _, ok := usedIds[vm.Id]; !ok {
			sp.allVideoMaps = append(sp.allVideoMaps, vm)
			usedIds[vm.Id] = nil
		}
	}

	// Make automatic built-in system maps
	asIdx := 700
	addAirspaceVolumes := func(label string, name string, filt sim.FilterRegions) {
		if len(filt) == 0 {
			return
		}

		// Add a map with all of them
		vm := sim.VideoMap{
			Label:    label,
			Name:     name,
			Id:       asIdx,
			Category: VideoMapProcessingAreas,
		}
		asIdx++
		for _, f := range filt {
			f.GenerateDrawCommands(&vm.CommandBuffer, ss.NmPerLongitude)
		}
		addMap(vm)

		for _, f := range filt {
			vm := sim.VideoMap{
				Label:    strings.ToUpper(f.Id),
				Name:     strings.ToUpper(f.Description),
				Id:       asIdx,
				Category: VideoMapProcessingAreas,
			}
			asIdx++
			f.GenerateDrawCommands(&vm.CommandBuffer, ss.NmPerLongitude)
			addMap(vm)
		}
	}
	addAirspaceVolumes("CASU", "CA SUPPRESSION AREA ALL", ss.STARSFacilityAdaptation.Filters.InhibitCA)
	addAirspaceVolumes("MSAWSU", "MSAW SUPPRESSION AREA ALL", ss.STARSFacilityAdaptation.Filters.InhibitMSAW)
	addAirspaceVolumes("ARRACQ", "ARRIVAL ACQUISITION AREA ALL", ss.STARSFacilityAdaptation.Filters.ArrivalAcquisition)
	addAirspaceVolumes("ARRDEP", "ARRIVAL DROP AREA ALL", ss.STARSFacilityAdaptation.Filters.ArrivalDrop)
	addAirspaceVolumes("DEPACQ", "DEPARTURE ACQUISITION AREA ALL", ss.STARSFacilityAdaptation.Filters.DepartureAcquisition)
	addAirspaceVolumes("SECDROP", "SECONDARY DROP AREA ALL", ss.STARSFacilityAdaptation.Filters.SecondaryDrop)
	addAirspaceVolumes("SURFTRK", "SURFACE TRACKING AREA ALL", ss.STARSFacilityAdaptation.Filters.SurfaceTracking)
	addAirspaceVolumes("QLRGNS", "QUICKLOOK REGIONS ALL", ss.STARSFacilityAdaptation.Filters.Quicklook)

	// MVAs
	mvas := sim.VideoMap{
		Label:    ss.TRACON + " MVA",
		Name:     "ALL MINIMUM VECTORING ALTITUDES",
		Id:       asIdx,
		Category: VideoMapProcessingAreas,
	}
	ld := renderer.GetLinesDrawBuilder()
	for _, mva := range av.DB.MVAs[ss.TRACON] {
		ld.AddLineLoop(mva.ExteriorRing)
		p := math.Extent2DFromPoints(mva.ExteriorRing).Center()
		ld.AddNumber(p, 0.005, fmt.Sprintf("%d", mva.MinimumLimit/100))
	}
	ld.GenerateCommands(&mvas.CommandBuffer)
	renderer.ReturnLinesDrawBuilder(ld)
	asIdx++
	addMap(mvas)

	// Nearby airspace definitions
	addAirspace := func(airspace map[string][]av.AirspaceVolume, class string) {
		for name, airspace := range airspace {
			if math.NMDistance2LL(airspace[0].PolygonBounds.ClosestPointInBox(ss.Center), ss.Center) > 75 {
				continue
			}

			amap := sim.VideoMap{
				Label:    name,
				Name:     name + " CLASS " + class,
				Id:       asIdx,
				Category: VideoMapProcessingAreas,
			}
			for _, asp := range airspace {
				asp.GenerateDrawCommands(&amap.CommandBuffer, ss.NmPerLongitude)
			}

			addMap(amap)
			asIdx++
		}
	}
	addAirspace(av.DB.BravoAirspace, "B")
	addAirspace(av.DB.CharlieAirspace, "C")

	// Radar maps
	radarIndex := 801
	for _, name := range util.SortedMapKeys(ss.STARSFacilityAdaptation.RadarSites) {
		sm := sim.VideoMap{
			Label:    name + "RCM",
			Name:     name + " RADAR COVERAGE MAP",
			Id:       radarIndex,
			Category: VideoMapProcessingAreas,
		}

		site := ss.STARSFacilityAdaptation.RadarSites[name]
		ld := renderer.GetLinesDrawBuilder()
		ld.AddLatLongCircle(site.Position, ss.NmPerLongitude, float32(site.PrimaryRange), 360)
		ld.AddLatLongCircle(site.Position, ss.NmPerLongitude, float32(site.SecondaryRange), 360)
		ld.GenerateCommands(&sm.CommandBuffer)
		addMap(sm)

		radarIndex++
		renderer.ReturnLinesDrawBuilder(ld)
	}

	// ATPA approach volumes
	atpaIndex := 901
	for _, name := range util.SortedMapKeys(ss.ArrivalAirports) {
		ap := ss.Airports[name]
		for _, rwy := range util.SortedMapKeys(ap.ATPAVolumes) {
			vol := ap.ATPAVolumes[rwy]

			label := "A" + name[1:] + rwy
			if len(label) > 7 {
				label = label[:7]
			}
			sm := sim.VideoMap{
				Label:    label,
				Name:     name + rwy + " ATPA APPROACH VOLUME",
				Id:       atpaIndex,
				Category: VideoMapProcessingAreas,
			}

			ld := renderer.GetLinesDrawBuilder()
			rect := vol.GetRect(ss.NmPerLongitude, ss.MagneticVariation)
			for i := range rect {
				ld.AddLine(rect[i], rect[(i+1)%len(rect)])
			}
			ld.GenerateCommands(&sm.CommandBuffer)

			addMap(sm)
			atpaIndex++
			renderer.ReturnLinesDrawBuilder(ld)
		}
	}

	// Start with the video maps associated with the Sim.
	sp.dcbVideoMaps = nil
	for _, name := range ss.ControllerVideoMaps {
		if idx := slices.IndexFunc(sp.allVideoMaps, func(v sim.VideoMap) bool { return v.Name == name }); idx != -1 && name != "" {
			sp.dcbVideoMaps = append(sp.dcbVideoMaps, &sp.allVideoMaps[idx])
		} else {
			sp.dcbVideoMaps = append(sp.dcbVideoMaps, nil)
		}
	}
}

func (sp *STARSPane) getVideoMapLibrary(ss sim.State, client *client.ControlClient) (*sim.VideoMapLibrary, error) {
	filename := ss.STARSFacilityAdaptation.VideoMapFile
	if ml, err := sim.HashCheckLoadVideoMap(filename, ss.VideoMapLibraryHash); err == nil {
		return ml, nil
	} else {
		return client.GetVideoMapLibrary(filename)
	}
}

func (sp *STARSPane) CanTakeKeyboardFocus() bool { return true }

func (sp *STARSPane) Upgrade(from, to int) {
	for _, prefs := range sp.TRACONPreferenceSets {
		prefs.Upgrade(from, to)
	}
	if sp.OldPrefsCurrentPreferenceSet != nil {
		sp.OldPrefsCurrentPreferenceSet.Upgrade(from, to)
	}
	if sp.OldPrefsSelectedPreferenceSet != nil && (*sp.OldPrefsSelectedPreferenceSet < 0 || *sp.OldPrefsSelectedPreferenceSet >= numSavedPreferenceSets) {
		sp.OldPrefsSelectedPreferenceSet = nil
	}
	for i := range sp.OldPrefsPreferenceSets {
		sp.OldPrefsPreferenceSets[i].Upgrade(from, to)
	}
}

func (sp *STARSPane) Draw(ctx *panes.Context, cb *renderer.CommandBuffer) {
	sp.processEvents(ctx)

	// Per-aircraft stuff: tracks, datablocks, vector lines, range rings, ...
	// Sort the aircraft so that they are always drawn in the same order
	// (go's map iterator randomization otherwise randomizes the order,
	// which can cause shimmering when datablocks overlap (especially if
	// one is selected). We'll go with alphabetical by callsign, with the
	// selected aircraft, if any, always drawn last.
	tracks := sp.visibleTracks(ctx)
	sort.Slice(tracks, func(i, j int) bool {
		return tracks[i].ADSBCallsign < tracks[j].ADSBCallsign
	})

	sp.updateRadarTracks(ctx, tracks)
	sp.autoReleaseDepartures(ctx)

	ps := sp.currentPrefs()

	// Clear to background color
	cb.ClearRGB(ps.Brightness.BackgroundContrast.ScaleRGB(STARSBackgroundColor))

	sp.processKeyboardInput(ctx, tracks)

	ctr := util.Select(ps.UseUserCenter, ps.UserCenter, ps.DefaultCenter)
	transforms := radar.GetScopeTransformations(ctx.PaneExtent, ctx.MagneticVariation, ctx.NmPerLongitude,
		ctr, float32(ps.Range), 0)

	scopeExtent := ctx.PaneExtent
	if ps.DisplayDCB {
		scopeExtent = sp.drawDCB(ctx, transforms, cb)

		// Update scissor for what's left and to protect the DCB (even
		// though this is apparently unrealistic, at least as far as radar
		// tracks go...)
		cb.SetScissorBounds(scopeExtent, ctx.Platform.FramebufferSize()[1]/ctx.Platform.DisplaySize()[1])
	}

	sp.drawWX(ctx, transforms, cb)

	sp.drawRangeRings(ctx, transforms, cb)

	sp.drawVideoMaps(ctx, transforms, cb)

	sp.drawScenarioRoutes(ctx, transforms, sp.systemFont(ctx, ps.CharSize.Tools), cb)

	sp.drawCRDARegions(ctx, transforms, cb)
	sp.drawSelectedRoute(ctx, transforms, cb)
	sp.drawPlotPoints(ctx, transforms, cb)
	sp.drawWind(ctx, transforms, cb)

	sp.drawCompass(ctx, scopeExtent, transforms, cb)

	sp.drawRestrictionAreas(ctx, transforms, cb)

	sp.drawSystemLists(ctx, tracks, ctx.PaneExtent, transforms, cb)

	sp.drawHistoryTrails(ctx, tracks, transforms, cb)

	sp.drawPTLs(ctx, tracks, transforms, cb)
	sp.drawRingsAndCones(ctx, tracks, transforms, cb)
	sp.drawRBLs(ctx, tracks, transforms, cb)
	sp.drawMinSep(ctx, transforms, cb)

	sp.drawHighlighted(ctx, transforms, cb)
	sp.drawVFRAirports(ctx, transforms, cb)

	dbs := sp.getAllDatablocks(ctx, tracks)
	sp.drawLeaderLines(ctx, tracks, dbs, transforms, cb)
	sp.drawTracks(ctx, tracks, transforms, cb)
	sp.drawDatablocks(tracks, dbs, ctx, transforms, cb)

	ghosts := sp.getGhostTracks(ctx, tracks)
	sp.drawGhosts(ctx, ghosts, transforms, cb)

	if ctx.Mouse != nil {
		// Is the mouse over the DCB or over the regular STARS scope? Note that
		// we need to offset the mouse position to be w.r.t. window coordinates
		// to match scopeExtent.
		mouseOverDCB := !scopeExtent.Inside(math.Add2f(ctx.Mouse.Pos, ctx.PaneExtent.P0))
		if !mouseOverDCB {
			// DCB buttons handle their own click checks, etc.
			sp.consumeMouseEvents(ctx, ghosts, transforms, tracks, cb)
		}
		sp.drawMouseCursor(ctx, mouseOverDCB, transforms, cb)
	}
	sp.handleCapture(ctx, transforms, cb)

	sp.updateAudio(ctx, tracks)

	// Do this at the end of drawing so that we hold on to the tracks we
	// have for rendering the current frame.
	if sp.discardTracks {
		for _, state := range sp.TrackState {
			state.historyTracksIndex = 0
		}
		sp.lastTrackUpdate = time.Time{} // force update
		sp.lastHistoryTrackUpdate = time.Time{}
		sp.discardTracks = false
	}

	sp.drawPauseOverlay(ctx, cb)
}

func (sp *STARSPane) drawPauseOverlay(ctx *panes.Context, cb *renderer.CommandBuffer) {
	if !ctx.Client.State.Paused {
		return
	}

	text := "SIMULATION PAUSED"
	font := sp.systemFontA[3] // Largest font

	// Get pane width
	width := ctx.PaneExtent.Width()
	height := ctx.PaneExtent.Height()

	// Fixed position from top
	topOffset := height - 140
	textY := topOffset + 30      // Text will be 30px below top (in middle of background quad)
	quadTop := topOffset + 45    // Background extends 15px above text
	quadBottom := topOffset + 15 // Background extends 15px below text

	// Draw background quad (fixed width of 360px centered horizontally)
	quad := renderer.GetColoredTrianglesDrawBuilder()
	defer renderer.ReturnColoredTrianglesDrawBuilder(quad)
	quad.AddQuad(
		[2]float32{width/2 - 180, quadTop},    // Left-top
		[2]float32{width/2 + 180, quadTop},    // Right-top
		[2]float32{width/2 + 180, quadBottom}, // Right-bottom
		[2]float32{width/2 - 180, quadBottom}, // Left-bottom
		renderer.RGB{R: 1, G: 0, B: 0})        // Solid red

	// Draw text
	td := renderer.GetTextDrawBuilder()
	defer renderer.ReturnTextDrawBuilder(td)
	td.AddTextCentered(text, [2]float32{width / 2, textY}, renderer.TextStyle{
		Font:  font,
		Color: renderer.RGB{R: 1, G: 1, B: 1},
	})

	// Apply transformations and draw
	transforms := radar.GetScopeTransformations(ctx.PaneExtent, 0, 0, [2]float32{}, 0, 0)
	transforms.LoadWindowViewingMatrices(cb)
	quad.GenerateCommands(cb)
	td.GenerateCommands(cb)
}

func (sp *STARSPane) drawWX(ctx *panes.Context, transforms radar.ScopeTransformations, cb *renderer.CommandBuffer) {
	ps := sp.currentPrefs()
	weatherBrightness := float32(ps.Brightness.Weather) / float32(100)
	weatherContrast := float32(ps.Brightness.WxContrast) / float32(100)

	if !sp.wxNextHistoryStepTime.IsZero() && ctx.Now.After(sp.wxNextHistoryStepTime) {
		sp.wxHistoryDraw--
		if sp.wxHistoryDraw > 0 {
			sp.wxNextHistoryStepTime = ctx.Now.Add(5 * time.Second)
		} else {
			sp.wxNextHistoryStepTime = time.Time{}
			if sp.previewAreaOutput == "IN PROGRESS" {
				sp.previewAreaOutput = ""
			}
		}
	}

	sp.weatherRadar.Draw(ctx, sp.wxHistoryDraw, weatherBrightness, weatherContrast, ps.DisplayWeatherLevel,
		transforms, cb)
}

const numMapColors = 8

var mapColors [2][numMapColors]renderer.RGB = [2][numMapColors]renderer.RGB{
	[numMapColors]renderer.RGB{ // Group A
		renderer.RGBFromUInt8(140, 140, 140),
		renderer.RGBFromUInt8(0, 255, 255),
		renderer.RGBFromUInt8(255, 0, 255),
		renderer.RGBFromUInt8(238, 201, 0),
		renderer.RGBFromUInt8(238, 106, 80),
		renderer.RGBFromUInt8(162, 205, 90),
		renderer.RGBFromUInt8(218, 165, 32),
		renderer.RGBFromUInt8(72, 118, 255),
	},
	[numMapColors]renderer.RGB{ // Group B
		renderer.RGBFromUInt8(140, 140, 140),
		renderer.RGBFromUInt8(132, 112, 255),
		renderer.RGBFromUInt8(118, 238, 198),
		renderer.RGBFromUInt8(237, 145, 33),
		renderer.RGBFromUInt8(218, 112, 214),
		renderer.RGBFromUInt8(238, 180, 180),
		renderer.RGBFromUInt8(50, 205, 50),
		renderer.RGBFromUInt8(255, 106, 106),
	},
}

func (sp *STARSPane) drawVideoMaps(ctx *panes.Context, transforms radar.ScopeTransformations, cb *renderer.CommandBuffer) {
	ps := sp.currentPrefs()

	transforms.LoadLatLongViewingMatrices(cb)

	cb.LineWidth(1, ctx.DPIScale)
	var draw []sim.VideoMap
	for _, vm := range sp.allVideoMaps {
		if _, ok := ps.VideoMapVisible[vm.Id]; ok {
			draw = append(draw, vm)
		}
	}
	slices.SortFunc(draw, func(a, b sim.VideoMap) int { return a.Id - b.Id })

	for _, vm := range draw {
		brite := util.Select(vm.Group == 0, ps.Brightness.VideoGroupA, ps.Brightness.VideoGroupB)
		cidx := math.Clamp(vm.Color-1, 0, numMapColors-1) // switch to 0-based indexing
		color := brite.ScaleRGB(mapColors[vm.Group][cidx])

		cb.SetRGB(color)
		cb.Call(vm.CommandBuffer)
	}
}

var restrictionAreaStipple [32]uint32 = [32]uint32{
	0b10001000100010001000100010001000,
	0,
	0b00100010001000100010001000100010,
	0,
	0b10001000100010001000100010001000,
	0,
	0b00100010001000100010001000100010,
	0,
	0b10001000100010001000100010001000,
	0,
	0b00100010001000100010001000100010,
	0,
	0b10001000100010001000100010001000,
	0,
	0b00100010001000100010001000100010,
	0,
	0b10001000100010001000100010001000,
	0,
	0b00100010001000100010001000100010,
	0,
	0b10001000100010001000100010001000,
	0,
	0b00100010001000100010001000100010,
	0,
	0b10001000100010001000100010001000,
	0,
	0b00100010001000100010001000100010,
	0,
	0b10001000100010001000100010001000,
	0,
	0b00100010001000100010001000100010,
	0,
}

var restrictionAreaHighDPIStipple [32]uint32 = [32]uint32{
	0b11000000110000001100000011000000,
	0b11000000110000001100000011000000,
	0,
	0,
	0b00001100000011000000110000001100,
	0b00001100000011000000110000001100,
	0,
	0,
	0b11000000110000001100000011000000,
	0b11000000110000001100000011000000,
	0,
	0,
	0b00001100000011000000110000001100,
	0b00001100000011000000110000001100,
	0,
	0,
	0b11000000110000001100000011000000,
	0b11000000110000001100000011000000,
	0,
	0,
	0b00001100000011000000110000001100,
	0b00001100000011000000110000001100,
	0,
	0,
	0b11000000110000001100000011000000,
	0b11000000110000001100000011000000,
	0,
	0,
	0b00001100000011000000110000001100,
	0b00001100000011000000110000001100,
	0,
	0,
}

func raGeomColor(ra *av.RestrictionArea) renderer.RGB {
	return [9]renderer.RGB{
		renderer.RGBFromUInt8(255, 255, 0), // double up so 0 by default remains yellow but we have 1-based indexing otherwise
		renderer.RGBFromUInt8(255, 255, 0),
		renderer.RGBFromUInt8(0, 255, 255),
		renderer.RGBFromUInt8(255, 0, 255),
		renderer.RGBFromUInt8(238, 201, 0),
		renderer.RGBFromUInt8(238, 106, 80),
		renderer.RGBFromUInt8(132, 112, 255),
		renderer.RGBFromUInt8(118, 238, 198),
		renderer.RGBFromUInt8(50, 205, 50),
	}[ra.Color]
}

func (sp *STARSPane) drawWIPRestrictionArea(ctx *panes.Context, transforms radar.ScopeTransformations, cb *renderer.CommandBuffer) {
	ra := sp.wipRestrictionArea
	if ra == nil {
		return
	}
	ld := renderer.GetLinesDrawBuilder()
	defer renderer.ReturnLinesDrawBuilder(ld)
	var trid *renderer.TrianglesDrawBuilder

	if ra.CircleRadius > 0 {
		if ra.Shaded {
			trid = renderer.GetTrianglesDrawBuilder()
			defer renderer.ReturnTrianglesDrawBuilder(trid)
			trid.AddLatLongCircle(ra.CircleCenter, ctx.NmPerLongitude, ra.CircleRadius, 90)
		}
		ld.AddLatLongCircle(ra.CircleCenter, ctx.NmPerLongitude, ra.CircleRadius, 90)
	} else if len(ra.Vertices) > 0 && len(ra.Vertices[0]) > 0 {
		verts := sp.wipRestrictionArea.Vertices[0]
		for i := range len(verts) - 1 {
			ld.AddLine(verts[i], verts[i+1])
		}

		if ctx.Mouse != nil {
			sp.wipRestrictionAreaMouseMoved = sp.wipRestrictionAreaMouseMoved ||
				(ctx.Mouse.Pos != sp.wipRestrictionAreaMousePos)
			// Only draw the line to the mouse cursor if it has moved since we started entering
			if sp.wipRestrictionAreaMouseMoved && sp.previewAreaInput == "" {
				pm := transforms.LatLongFromWindowP(ctx.Mouse.Pos)
				ld.AddLine(verts[len(verts)-1], pm)
			}
		}
	}

	transforms.LoadLatLongViewingMatrices(cb)
	cb.LineWidth(1, ctx.DPIScale)
	ps := sp.currentPrefs()
	color := ps.Brightness.VideoGroupB.ScaleRGB(renderer.RGB{1, 1, 1})
	cb.SetRGB(color)

	ld.GenerateCommands(cb)
	if trid != nil {
		cb.EnablePolygonStipple()
		trid.GenerateCommands(cb)
		cb.DisablePolygonStipple()
	}
}

func (sp *STARSPane) drawRestrictionAreas(ctx *panes.Context, transforms radar.ScopeTransformations, cb *renderer.CommandBuffer) {
	sp.drawWIPRestrictionArea(ctx, transforms, cb)

	ps := sp.currentPrefs()
	draw := make(map[int]*av.RestrictionArea)
	for idx, s := range ps.RestrictionAreaSettings {
		if !s.Visible {
			continue
		}

		if ra := getRestrictionAreaByIndex(ctx, idx); ra != nil {
			draw[idx] = ra
		}
	}

	if len(draw) == 0 {
		return
	}

	transforms.LoadLatLongViewingMatrices(cb)
	cb.LineWidth(1, ctx.DPIScale)

	// Draw the geometric bits before the text
	ld := renderer.GetLinesDrawBuilder()
	defer renderer.ReturnLinesDrawBuilder(ld)
	trid := renderer.GetTrianglesDrawBuilder()
	defer renderer.ReturnTrianglesDrawBuilder(trid)

	if ctx.DPIScale > 1.5 {
		cb.PolygonStipple(restrictionAreaHighDPIStipple)
	} else {
		cb.PolygonStipple(restrictionAreaStipple)
	}

	for _, idx := range util.SortedMapKeys(draw) {
		ra := draw[idx]

		ld.Reset()
		trid.Reset()

		color := ps.Brightness.VideoGroupB.ScaleRGB(raGeomColor(ra))
		cb.SetRGB(color)

		if ra.CircleRadius > 0 {
			if ra.Shaded {
				trid.AddLatLongCircle(ra.CircleCenter, ctx.NmPerLongitude, ra.CircleRadius, 90)
			}
			ld.AddLatLongCircle(ra.CircleCenter, ctx.NmPerLongitude, ra.CircleRadius, 90)
		} else {
			for _, loop := range ra.Vertices {
				if nv := len(loop); nv > 0 {
					for i := range nv - 1 {
						ld.AddLine(loop[i], loop[i+1])
					}
					if ra.Closed {
						ld.AddLine(loop[nv-1], loop[0])
					}
				}
			}
			if ra.Shaded {
				for _, tri := range ra.Tris {
					trid.AddTriangle(tri[0], tri[1], tri[2])
				}
			}
		}
		if ra.Shaded {
			cb.EnablePolygonStipple()
			trid.GenerateCommands(cb)
			cb.DisablePolygonStipple()
		}
		ld.GenerateCommands(cb)
	}

	// Draw text
	td := renderer.GetTextDrawBuilder()
	defer renderer.ReturnTextDrawBuilder(td)
	font := sp.systemFont(ctx, ps.CharSize.Lists)
	halfSeconds := ctx.Now.UnixMilli() / 500
	blinkDim := halfSeconds&1 == 0
	color := ps.Brightness.VideoGroupB.ScaleRGB(renderer.RGB{1, 1, 0}) // always yellow

	for _, idx := range util.SortedMapKeys(draw) {
		ra := draw[idx]
		var text string
		if !ra.HideId {
			text = fmt.Sprintf("[%d]", idx)
		}

		settings := ps.RestrictionAreaSettings[idx]
		if ra.Text[0] != "" && !settings.HideText {
			indent := len(text)
			text += strings.ToUpper(ra.Text[0])
			if ra.Text[1] != "" {
				text += "\n"
				if indent > 0 {
					text += fmt.Sprintf("%*c", indent, ' ')
				}
				text += strings.ToUpper(ra.Text[1])
			}
		}

		p := transforms.WindowFromLatLongP(ra.TextPosition)
		blinking := settings.ForceBlinkingText || (ra.BlinkingText && !settings.StopBlinkingText)
		if blinking && blinkDim {
			td.AddTextCentered(text, p, renderer.TextStyle{Font: font, Color: color.Scale(0.5)})
		} else {
			td.AddTextCentered(text, p, renderer.TextStyle{Font: font, Color: color})
		}
	}
	transforms.LoadWindowViewingMatrices(cb)
	td.GenerateCommands(cb)
}

func (sp *STARSPane) drawCRDARegions(ctx *panes.Context, transforms radar.ScopeTransformations, cb *renderer.CommandBuffer) {
	transforms.LoadLatLongViewingMatrices(cb)

	ps := sp.currentPrefs()
	for i, state := range ps.CRDA.RunwayPairState {
		for j, rwyState := range state.RunwayState {
			if rwyState.DrawCourseLines {
				region := sp.ConvergingRunways[i].ApproachRegions[j]
				line, _ := region.GetLateralGeometry(ctx.NmPerLongitude, ctx.MagneticVariation)

				ld := renderer.GetLinesDrawBuilder()
				cb.SetRGB(ps.Brightness.OtherTracks.ScaleRGB(STARSGhostColor))
				ld.AddLine(line[0], line[1])

				ld.GenerateCommands(cb)
				renderer.ReturnLinesDrawBuilder(ld)
			}

			if rwyState.DrawQualificationRegion {
				region := sp.ConvergingRunways[i].ApproachRegions[j]
				_, quad := region.GetLateralGeometry(ctx.NmPerLongitude, ctx.MagneticVariation)

				ld := renderer.GetLinesDrawBuilder()
				cb.SetRGB(ps.Brightness.OtherTracks.ScaleRGB(STARSGhostColor))
				ld.AddLineLoop([][2]float32{quad[0], quad[1], quad[2], quad[3]})

				ld.GenerateCommands(cb)
				renderer.ReturnLinesDrawBuilder(ld)
			}
		}
	}
}

func (sp *STARSPane) drawMouseCursor(ctx *panes.Context, mouseOverDCB bool, transforms radar.ScopeTransformations,
	cb *renderer.CommandBuffer) {
	ps := sp.currentPrefs()
	td := renderer.GetTextDrawBuilder()
	defer renderer.ReturnTextDrawBuilder(td)

	if mouseOverDCB {
		return
	}

	ctx.Mouse.SetCursor(imgui.MouseCursorNone)

	if sp.hideMouseCursor { // auto home
		if ctx.Mouse != nil && ctx.Mouse.Pos != ps.CursorHome {
			sp.hideMouseCursor = false // it moved
		}
		if sp.hideMouseCursor {
			return
		}
	}

	// Don't draw the cursor with command modes that capture the mouse
	if sp.activeSpinner != nil {
		return
	}
	if sp.commandMode == CommandModePlaceCenter {
		return
	}

	// STARS Operators Manual 4-74: FDB brightness is used for the cursor
	cursorStyle := renderer.TextStyle{Font: sp.cursorsFont, Color: ps.Brightness.FullDatablocks.RGB()}
	background := ps.Brightness.BackgroundContrast.ScaleRGB(STARSBackgroundColor)
	bgStyle := renderer.TextStyle{Font: sp.cursorsFont, Color: background}

	draw := func(idx int, style renderer.TextStyle) {
		p := [2]float32{float32(int(ctx.Mouse.Pos[0] + 0.5)), float32(int(ctx.Mouse.Pos[1] + 0.5))}
		td.AddText(string(byte(idx)), p, style)
	}
	// The STARS "+" cursors start at 0 in the STARS cursors font,
	// ordered by size. There is no cursor for size 5, so we'll use 4 for that.
	// The second of the two is the background one
	// that establishes a mask.
	idx := 2 * min(4, ps.CharSize.Datablocks)
	draw(idx+1, bgStyle)
	draw(idx, cursorStyle)

	cb.SetDrawBounds(ctx.PaneExtent, ctx.Platform.FramebufferSize()[1]/ctx.Platform.DisplaySize()[1])
	transforms.LoadWindowViewingMatrices(cb)
	td.GenerateCommands(cb)
}

func (sp *STARSPane) makeSignificantPoints(ss sim.State) {
	sp.significantPoints = util.DuplicateMap(ss.STARSFacilityAdaptation.SignificantPoints)
	sp.significantPointsSlice = nil
	for _, pt := range sp.significantPoints {
		sp.significantPointsSlice = append(sp.significantPointsSlice, pt)
	}

	tryAdd := func(name string, desc string, loc math.Point2LL) {
		if _, ok := sp.significantPoints[name]; ok {
			return
		}

		pt := sim.SignificantPoint{
			Name:        name,
			Description: desc,
			Location:    loc,
		}
		sp.significantPoints[name] = pt
		sp.significantPointsSlice = append(sp.significantPointsSlice, pt)
	}

	// All airports within 250nm
	center := ss.GetInitialCenter()
	for name, ap := range av.DB.Airports {
		if math.NMDistance2LL(ap.Location, center) < 250 {
			if len(name) == 4 && name[0] == 'K' {
				name = name[1:]
			}
			tryAdd(name, name+" AIRPORT", ap.Location)

			for _, rwy := range ap.Runways {
				// e.g. JFK22LT -> JFK RWY 22L THRESHOLD
				tryAdd(name+rwy.Id+"T", name+" RWY "+rwy.Id+" THRESHOLD", rwy.Threshold)
			}
		}
	}

	for name, nav := range av.DB.Navaids {
		if math.NMDistance2LL(nav.Location, center) < 250 {
			tryAdd(name, name+" "+nav.Type, nav.Location)
		}
	}

	for name, fix := range av.DB.Fixes {
		if math.NMDistance2LL(fix.Location, center) < 250 {
			// FIXME: should be INTERSECTION not WAYPOINT potentially
			tryAdd(name, name+" WAYPOINT", fix.Location)
		}
	}

	// Sort the slice
	slices.SortFunc(sp.significantPointsSlice, func(a, b sim.SignificantPoint) int {
		return strings.Compare(a.Name, b.Name)
	})
}

const (
	RadarModeSingle = iota
	RadarModeMulti
	RadarModeFused
)

func (sp *STARSPane) radarMode(radarSites map[string]*av.RadarSite) int {
	if len(radarSites) == 0 {
		// Straight-up fused mode if none are specified.
		return RadarModeFused
	}

	ps := sp.currentPrefs()
	if _, ok := radarSites[ps.RadarSiteSelected]; ps.RadarSiteSelected != "" && ok {
		return RadarModeSingle
	} else if ps.FusedRadarMode {
		return RadarModeFused
	} else {
		return RadarModeMulti
	}
}

func (sp *STARSPane) visibleTracks(ctx *panes.Context) []sim.Track {
	var tracks []sim.Track
	ps := sp.currentPrefs()
	single := sp.radarMode(ctx.FacilityAdaptation.RadarSites) == RadarModeSingle
	now := ctx.Client.CurrentTime()

	for _, trk := range ctx.Client.State.Tracks {
		visible := false
		state := sp.TrackState[trk.ADSBCallsign]

		if trk.IsUnsupportedDB() {
			visible = true
		} else if sp.radarMode(ctx.FacilityAdaptation.RadarSites) == RadarModeFused {
			// If it hasn't been culled server-side due to e.g. the surface tracking filters,
			// we can see it.
			visible = true
		} else {
			// Otherwise see if any of the radars can see it
			for id, site := range ctx.FacilityAdaptation.RadarSites {
				if single && ps.RadarSiteSelected != id {
					continue
				}

				if p, s, _ := site.CheckVisibility(trk.Location, int(trk.TrueAltitude)); p || s {
					visible = true
				}
			}
		}

		if visible {
			tracks = append(tracks, *trk)

			// Is this the first we've seen it?
			if state.FirstRadarTrackTime.IsZero() {
				state.FirstRadarTrackTime = now
			}
		}
	}

	return tracks
}

func (sp *STARSPane) radarSiteId(radarSites map[string]*av.RadarSite) string {
	switch sp.radarMode(radarSites) {
	case RadarModeSingle:
		return sp.currentPrefs().RadarSiteSelected
	case RadarModeMulti:
		return "MULTI"
	case RadarModeFused:
		return "FUSED"
	default:
		return "UNKNOWN"
	}
}

func (sp *STARSPane) setRadarModeMulti() {
	ps := sp.currentPrefs()

	ps.RadarSiteSelected = ""
	if ps.FusedRadarMode {
		sp.discardTracks = true
		ps.FusedRadarMode = false
	}
}

func (sp *STARSPane) setRadarModeFused() {
	ps := sp.currentPrefs()

	ps.RadarSiteSelected = ""
	if !ps.FusedRadarMode {
		ps.FusedRadarMode = true
		sp.discardTracks = true
	}
}

func (sp *STARSPane) initializeAudio(p platform.Platform, lg *log.Logger) {
	if sp.audioEffects == nil {
		sp.audioEffects = make(map[AudioType]int)

		loadMP3 := func(filename string) int {
			idx, err := p.AddMP3(util.LoadResourceBytes("audio/" + filename))
			if err != nil {
				lg.Errorf("%s: %v", filename, err)
			}
			return idx
		}

		sp.audioEffects[AudioConflictAlert] = loadMP3("CA_1000ms.mp3")
		sp.audioEffects[AudioSquawkSPC] = loadMP3("SPC_700ms.mp3")
		sp.audioEffects[AudioMinimumSafeAltitudeWarning] = loadMP3("MSAW_1000ms.mp3")
		sp.audioEffects[AudioModeCIntruder] = loadMP3("MCI_1000ms.mp3")
		sp.audioEffects[AudioTest] = loadMP3("TEST_250ms.mp3")
		sp.audioEffects[AudioInboundHandoff] = loadMP3("263124__pan14__sine-octaves-up-beep.mp3")
		sp.audioEffects[AudioCommandError] = loadMP3("ERROR.mp3")
		sp.audioEffects[AudioHandoffAccepted] = loadMP3("321104__nsstudios__blip2.mp3")
	}
}

func (sp *STARSPane) playOnce(p platform.Platform, a AudioType) {
	if sp.currentPrefs().AudioEffectEnabled[a] {
		p.PlayAudioOnce(sp.audioEffects[a])
	}
}

const AlertAudioDuration = 5 * time.Second

func (sp *STARSPane) updateAudio(ctx *panes.Context, tracks []sim.Track) {
	ps := sp.currentPrefs()

	if !sp.testAudioEndTime.IsZero() && ctx.Now.After(sp.testAudioEndTime) {
		ctx.Platform.StopPlayAudio(sp.audioEffects[AudioTest])
		sp.testAudioEndTime = time.Time{}
	}

	updateContinuous := func(play bool, effect AudioType) {
		if ps.AudioEffectEnabled[effect] && play {
			ctx.Platform.StartPlayAudioContinuous(sp.audioEffects[effect])
		} else {
			ctx.Platform.StopPlayAudio(sp.audioEffects[effect])
		}
	}

	// Play the CA sound if any CAs or MSAWs are unacknowledged
	playCASound := false
	if !ps.DisableCAWarnings {
		playCASound = slices.ContainsFunc(sp.CAAircraft,
			func(ca CAAircraft) bool {
				if ca.Acknowledged {
					return false
				}
				trk0, ok0 := ctx.GetTrackByCallsign(ca.ADSBCallsigns[0])
				trk1, ok1 := ctx.GetTrackByCallsign(ca.ADSBCallsigns[0])
				if !ok0 || !ok1 {
					return false
				}
				return trk0.IsAssociated() && !trk0.FlightPlan.DisableCA &&
					trk1.IsAssociated() && !trk1.FlightPlan.DisableCA &&
					ctx.Now.Before(ca.SoundEnd)
			})
		playCASound = playCASound || slices.ContainsFunc(sp.MCIAircraft,
			func(ca CAAircraft) bool {
				if ca.Acknowledged {
					return false
				}
				trk0, ok := ctx.GetTrackByCallsign(ca.ADSBCallsigns[0])
				return ok && trk0.IsAssociated() && !trk0.FlightPlan.DisableCA &&
					ctx.Now.Before(ca.SoundEnd)
			})
	}
	updateContinuous(playCASound, AudioConflictAlert)

	playMSAWSound := !ps.DisableMSAW && func() bool {
		for _, trk := range tracks {
			if trk.IsUnassociated() || trk.FlightPlan.DisableMSAW {
				return false
			}
			state := sp.TrackState[trk.ADSBCallsign]
			if state.MSAW && !state.MSAWAcknowledged && !state.InhibitMSAW &&
				ctx.Now.Before(state.MSAWSoundEnd) {
				return true
			}
		}
		return false
	}()
	updateContinuous(playMSAWSound, AudioMinimumSafeAltitudeWarning)

	// 2-100: play sound if:
	// - There is an unacknowledged SPC in a track's datablock
	// - [todo]: track is unassociated or is associated and was displaying FDB
	// - [todo]: if unassociated, is on-screen or within an adapted distance
	playSPCSound := func() bool {
		for _, trk := range tracks {
			state := sp.TrackState[trk.ADSBCallsign]
			ok, _ := trk.Squawk.IsSPC()
			if ok && !state.SPCAcknowledged && ctx.Now.Before(state.SPCSoundEnd) {
				return true
			}
		}
		return false
	}()
	updateContinuous(playSPCSound, AudioSquawkSPC)
}

func (sp *STARSPane) handleCapture(ctx *panes.Context, transforms radar.ScopeTransformations, cb *renderer.CommandBuffer) {
	if !sp.capture.enabled {
		return
	}

	readPixels := func() *image.RGBA {
		// Window coords -> fb coords, also accounting for retina 2x
		p0 := math.Add2f(sp.capture.region[0], ctx.PaneExtent.P0)
		p1 := math.Add2f(sp.capture.region[1], ctx.PaneExtent.P0)
		p0, p1 = math.Scale2f(p0, 2), math.Scale2f(p1, 2)

		x := int(min(p0[0], p1[0]))
		y := int(min(p0[1], p1[1]))
		w := int(max(p0[0], p1[0])) - x
		h := int(max(p0[1], p1[1])) - y
		px := ctx.Renderer.ReadPixelRGBAs(x, y, w, h)

		// Flip in y
		for i := range h / 2 {
			for j := range 4 * w {
				a, b := 4*w*i+j, 4*w*(h-1-i)+j
				px[a], px[b] = px[b], px[a]
			}
		}
		// Alpha to 1
		for i := range h {
			for j := range w {
				px[4*w*i+4*j+3] = 255
			}
		}

		return &image.RGBA{
			Pix:    px,
			Stride: 4 * w,
			Rect: image.Rectangle{
				Min: image.Point{X: x, Y: y},
				Max: image.Point{X: x + w, Y: y + h},
			},
		}
	}

	if sp.capture.doStill && sp.capture.haveRegion {
		fn := "capture.png"
		if d, err := os.UserHomeDir(); err == nil {
			fn = d + "/" + fn
		}
		w, err := os.Create(fn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		} else {
			img := readPixels()
			if err = png.Encode(w, img); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
			}
			w.Close()
		}
		sp.capture.doStill = false
	} else if sp.capture.doVideo && sp.capture.haveRegion {
		if sp.capture.video.frameCh == nil {
			// Starting a new capture
			sp.capture.video.frameCh = make(chan *image.RGBA, 100)
			sp.capture.video.lastFrame = time.Time{}
			go captureEncodeFrames(sp.capture.video.frameCh)
		}
		if time.Since(sp.capture.video.lastFrame) > 95*time.Millisecond {
			sp.capture.video.lastFrame = time.Now()
			sp.capture.video.frameCh <- readPixels()
		}
	} else if !sp.capture.doVideo && sp.capture.video.frameCh != nil {
		// Finish the capture
		close(sp.capture.video.frameCh)
		sp.capture.video.frameCh = nil
	}

	if sp.capture.specifyingRegion || sp.capture.haveRegion {
		p0, p1 := sp.capture.region[0], sp.capture.region[1]
		if sp.capture.specifyingRegion && ctx.Mouse != nil {
			p1 = ctx.Mouse.Pos
		}
		// Offset the outline so it isn't included in the capture
		p0[0], p1[0] = min(p0[0], p1[0])-1, max(p0[0], p1[0])+1
		p0[1], p1[1] = min(p0[1], p1[1])-1, max(p0[1], p1[1])+1

		ld := renderer.GetLinesDrawBuilder()
		defer renderer.ReturnLinesDrawBuilder(ld)

		ld.AddLineLoop([][2]float32{p0, [2]float32{p0[0], p1[1]}, p1, [2]float32{p1[0], p0[1]}})
		transforms.LoadWindowViewingMatrices(cb)
		cb.SetRGB(renderer.RGB{R: 0, G: 0.75, B: 0.75})
		ld.GenerateCommands(cb)
		cb.DisableBlend()
	}
}

// captureEncodeFrames runs in a goroutine that is launched when a video
// capture is initiated.  It reads images from the given chan and writes
// out an animated GIF when the chan is closed.
func captureEncodeFrames(ch chan *image.RGBA) {
	// Store regular and 2x resolution for retina displays.
	gifs := [2]*gif.GIF{&gif.GIF{}, &gif.GIF{}}
	// Though we could have a unique palette per frame, we only need a
	// handful of colors and having a shared one allows us to check for
	// image equivalence by just comparing the pixels' palette index
	// values.
	var palette []color.RGBA

	for {
		if img := <-ch; img != nil {
			nx, ny := img.Bounds().Max.X-img.Bounds().Min.X, img.Bounds().Max.Y-img.Bounds().Min.Y
			pal := [2]*image.Paletted{
				&image.Paletted{
					Pix:    make([]uint8, nx/2*ny/2),
					Stride: nx / 2,
					Rect:   image.Rectangle{Max: image.Point{X: nx / 2, Y: ny / 2}},
				},
				&image.Paletted{
					Pix:    make([]uint8, nx*ny),
					Stride: nx,
					Rect:   image.Rectangle{Max: image.Point{X: nx, Y: ny}},
				},
			}

			for y := range ny {
				for x := range nx {
					offset := 4 * (x + y*nx)
					r, g, b, a := img.Pix[offset], img.Pix[offset+1], img.Pix[offset+2], img.Pix[offset+3]

					// Find the pixel's color in the palette or add it to
					// the palette if it's not there.
					idx := -1
					// Simple linear search; we only have a few colors in
					// practice so this should be fine.
					for i, c := range palette {
						if c.R == r && c.G == g && c.B == b && c.A == a {
							idx = i
							break
						}
					}
					if idx == -1 {
						idx = len(palette)
						palette = append(palette, color.RGBA{R: r, G: g, B: b, A: a})
					}
					if idx > 255 {
						panic("too many colors")
					}

					pal[1].Pix[x+y*nx] = uint8(idx)

					if x&1 == 0 && y&1 == 0 {
						// The downsampled image is done via simple point
						// sampling. Since MSAA is disabled anyway, this
						// should be fine.
						pal[0].Pix[x/2+y/2*nx/2] = uint8(idx)
					}
				}
			}

			for i := range 2 {
				if n := len(gifs[i].Image); n > 0 && slices.Equal(pal[i].Pix, gifs[i].Image[n-1].Pix) {
					// If the new frame matches the last one added, just
					// increase the last frame's display time by another
					// 100ms rather than duplicating it.
					gifs[i].Delay[n-1] += 10
				} else {
					// The image has changed, so add it to the GIF.
					for _, c := range palette {
						pal[i].Palette = append(pal[i].Palette, c)
					}
					gifs[i].Image = append(gifs[i].Image, pal[i])
					gifs[i].Delay = append(gifs[i].Delay, 10 /* 100ths of seconds */)
				}
			}
		} else {
			// No more images; save the animated GIFs.
			for i := range 2 {
				fn := [2]string{"capture.gif", "capture-2x.gif"}[i]
				if d, err := os.UserHomeDir(); err == nil {
					fn = d + "/" + fn
				}
				w, err := os.Create(fn)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%v\n", err)
				} else {
					if n := len(gifs[i].Image); n > 3 {
						// Drop the first and last image so that all of the
						// ones we keep have been visible for their full
						// time-slice.
						gifs[i].Image = gifs[i].Image[1 : n-1]
						gifs[i].Delay = gifs[i].Delay[1 : n-1]
					}

					if err := gif.EncodeAll(w, gifs[i]); err != nil {
						fmt.Fprintf(os.Stderr, "%v\n", err)
					}
					w.Close()
				}
				fmt.Printf("saved %s; %d frames\n", fn, len(gifs[i].Image))
			}
			return
		}
	}
}
