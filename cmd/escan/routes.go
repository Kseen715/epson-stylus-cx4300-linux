package main

import "net/http"

// Every endpoint this server has, in the order they are worth reading in. The
// mux is built from this table and so is the API document at /api/openapi.json,
// which is what keeps the two honest about each other. Adding an endpoint means
// adding a line here; there is nowhere else to add one.
//
// The static files - the pages themselves, the stylesheet and the icon - are
// not endpoints and are not listed.
func (s *server) routes(guard *auth) []route {
	rt := []route{{
		Method: http.MethodGet, Pattern: "/api/status", Handler: s.handleStatus,
		Summary: "What the server and the device are",
		Desc: "Answers 200 even when the scanner cannot be opened: the failure is in the " +
			"error field, because the page still needs the bed size and the resolutions " +
			"to render itself.",
		Resp: statusResponse{},
	}, {
		Method: http.MethodPost, Pattern: "/api/preview", Handler: s.handlePreview,
		Summary: "Start a low-resolution preview",
		Desc: "Returns as soon as the scan is accepted. The image, the progress and any " +
			"failure arrive on /api/events, so a preview started on one device is fully " +
			"visible on the others. With no body, or with full set, the whole bed is scanned.",
		Req: scanRequest{}, Resp: statusMessage{}, Code: http.StatusAccepted,
		Example: scanRequest{DPI: 75, Full: true},
		Other:   []status{{http.StatusConflict, "a scan is already running"}},
	}, {
		Method: http.MethodPost, Pattern: "/api/scan", Handler: s.handleScan,
		Summary: "Start a full-resolution scan",
		Desc: "As /api/preview, but at the scan resolution and saved to the output " +
			"location at full size. Without full, and without a rectangle, the shared " +
			"selection is used.",
		Req: scanRequest{}, Resp: statusMessage{}, Code: http.StatusAccepted,
		// A5 out of the top left corner, at 300 dpi: units of 1/600 inch.
		Example: scanRequest{DPI: 300, W: 3496, H: 4960},
		Other:   []status{{http.StatusConflict, "a scan is already running"}},
	}, {
		Method: http.MethodPost, Pattern: "/api/cancel", Handler: s.handleCancel,
		Summary: "Discard the scan in progress",
		Desc: "The scanner is left to finish its sweep: cutting the transfer short wedges " +
			"this device until its mains lead is pulled. So the image is dropped and the " +
			"page released, and the stage says the carriage is still moving.",
		Resp: statusMessage{},
	}, {
		Method: http.MethodGet, Pattern: "/api/state", Handler: s.handleState,
		Summary: "The shared state, once",
		Desc:    "The same snapshot /api/events pushes. Useful to a script; a page should watch the stream.",
		Resp:    snapshot{},
	}, {
		Method: http.MethodGet, Pattern: "/api/events", Handler: s.hub.handleEvents,
		Summary: "The shared state, as it changes",
		Desc: "Server-sent events. Every message is a whole snapshot rather than a delta, " +
			"so a browser that reconnects or missed a frame is correct again immediately. " +
			"A comment line is sent every 25 seconds to keep proxies from closing an idle " +
			"stream.",
		Resp: snapshot{}, Type: "text/event-stream",
	}, {
		Method: http.MethodGet, Pattern: "/api/live", Handler: s.handleLive,
		Summary: "The scan in progress, row by row",
		Desc: "A binary stream of the shrunk image as it is scanned, so a page can watch it " +
			"appear instead of staring at a progress bar. Big-endian throughout: a 16-byte " +
			"header of \"ESCL\", generation, width and height, then bands of " +
			"first row, row count and that many rows of 3-byte RGB pixels. The generation " +
			"changes per scan, and the stream ends rather than mixing two images together.",
		Type: "application/octet-stream",
		Other: []status{
			{http.StatusNoContent, "no scan has been started"},
			{http.StatusInternalServerError, "this connection cannot be streamed"},
		},
	}, {
		Method: http.MethodPost, Pattern: "/api/selection", Handler: s.handleSelection,
		Summary: "Share the crop rectangle",
		Desc: "In device units of 1/600 inch, which is what makes a rectangle dragged on " +
			"one screen mean the same thing on another. Send null, or a rectangle with no " +
			"width or height, for the whole bed.",
		Req: (*area)(nil), Resp: statusMessage{}, Example: area{X: 600, Y: 600, W: 3496, H: 4960},
		Other: []status{{http.StatusBadRequest, "malformed selection"}},
	}, {
		Method: http.MethodGet, Pattern: "/api/image/", Handler: s.handleImage,
		Summary: "The displayed copy of a preview or a scan",
		Desc: "The shrunk PNG the pages draw, by the id carried in the snapshot. An id is " +
			"minted once and never reused, so the answer is cached for good. Only the " +
			"current preview and the current result are kept.",
		Param: &param{"id", "image id from the preview, result or last field of a snapshot"},
		Type:  "image/png",
		Other: []status{{http.StatusNotFound, "no image with that id"}},
	}, {
		Method: http.MethodGet, Pattern: "/api/file/", Handler: s.handleFile,
		Summary: "A saved scan, at full resolution",
		Desc: "Serves the file behind the savedName of a snapshot from the output location, " +
			"as an attachment. Only a plain file name is accepted.",
		Param: &param{"name", "savedName from a snapshot; no directories"},
		Type:  "image/png",
		Other: []status{{http.StatusNotFound, "no such scan, or the name is not a plain file name"}},
	}, {
		Method: http.MethodPost, Pattern: "/api/reset", Handler: s.handleReset,
		Summary: "Reset the scanner over USB",
		Desc: "A port reset, which is what recovers a wedged device without pulling its " +
			"mains lead. Only the Linux backend can do it; canReset in /api/status says whether.",
		Resp: statusMessage{},
		Other: []status{
			{http.StatusConflict, "a scan is running"},
			{http.StatusNotImplemented, "this backend cannot reset the scanner"},
			{http.StatusServiceUnavailable, "the scanner could not be opened"},
			{http.StatusInternalServerError, "the reset failed"},
		},
	}}

	if guard != nil {
		rt = append(rt, route{
			Method: http.MethodPost, Pattern: "/api/login", Handler: guard.handleLogin,
			Summary: "Exchange the configured credentials for tokens",
			Desc: "Sets both tokens as cookies and returns them in the body as well, for a " +
				"script that would rather send Authorization: Bearer. A wrong user and a " +
				"wrong password are reported identically.",
			Req: loginRequest{}, Resp: tokenResponse{},
			Example: loginRequest{User: "scan", Password: "..."},
			Other: []status{
				{http.StatusBadRequest, "malformed request"},
				{http.StatusUnauthorized, "wrong user name or password"},
			},
		}, route{
			Method: http.MethodPost, Pattern: "/api/refresh", Handler: guard.handleRefresh,
			Summary: "Trade a refresh token for a new pair",
			Desc: "A browser never needs this - a valid refresh cookie renews the access " +
				"token in passing on any request - but a script holding a bearer token does, " +
				"and presents the refresh token in the Authorization header here.",
			Resp:  tokenResponse{},
			Other: []status{{http.StatusUnauthorized, "the refresh token is missing, expired or forged"}},
		}, route{
			Method: http.MethodPost, Pattern: "/api/logout", Handler: guard.handleLogout,
			Summary: "Clear both cookies",
			Desc:    "A bearer token is not revoked by this; it simply expires.",
			Resp:    statusMessage{},
		})
	}

	return append(rt, route{
		Method: http.MethodGet, Pattern: "/api/openapi.json", Handler: s.handleOpenAPI,
		Summary: "This document",
		Desc: "Generated from the route table and from the Go types the handlers decode and " +
			"encode, so it describes what the server does rather than what someone wrote " +
			"down. Whether an endpoint needs a login is shown only when this server was " +
			"started with authentication on.",
		Type: "application/json",
	}, route{
		Method: http.MethodGet, Pattern: "/docs", Handler: s.handleDocs,
		Summary: "The page that renders this document",
		Type:    "text/html",
	})
}
