//go:build linux

package main

/*
#include "sane_abi.h"
*/
import "C"

// Go names for the SANE ABI types and for the constants used away from the
// entry points. They are aliases, not conversions, so they are the very same
// types the exported functions take - this just keeps cgo confined to the
// files that genuinely need it, and lets the tests compile, since a test file
// may not import "C".
type (
	saneWord   = C.SANE_Word
	saneInt    = C.SANE_Int
	saneFixed  = C.SANE_Fixed
	saneBool   = C.SANE_Bool
	saneStatus = C.SANE_Status
	saneAction = C.SANE_Action
	saneRange  = C.SANE_Range
	saneOption = C.SANE_Option_Descriptor
)

const (
	statusGood         = C.SANE_STATUS_GOOD
	statusInval        = C.SANE_STATUS_INVAL
	statusUnsupported  = C.SANE_STATUS_UNSUPPORTED
	statusCancelled    = C.SANE_STATUS_CANCELLED
	statusDeviceBusy   = C.SANE_STATUS_DEVICE_BUSY
	statusEOF          = C.SANE_STATUS_EOF
	statusIOError      = C.SANE_STATUS_IO_ERROR
	statusAccessDenied = C.SANE_STATUS_ACCESS_DENIED

	actionGet  = C.SANE_ACTION_GET_VALUE
	actionSet  = C.SANE_ACTION_SET_VALUE
	actionAuto = C.SANE_ACTION_SET_AUTO

	infoInexact       = C.SANE_INFO_INEXACT
	infoReloadOptions = C.SANE_INFO_RELOAD_OPTIONS
	infoReloadParams  = C.SANE_INFO_RELOAD_PARAMS

	typeBool   = C.SANE_TYPE_BOOL
	typeInt    = C.SANE_TYPE_INT
	typeFixed  = C.SANE_TYPE_FIXED
	typeGroup  = C.SANE_TYPE_GROUP
	typeString = C.SANE_TYPE_STRING

	unitDPI = C.SANE_UNIT_DPI
	unitMM  = C.SANE_UNIT_MM

	capSoftSelect = C.SANE_CAP_SOFT_SELECT
	capSoftDetect = C.SANE_CAP_SOFT_DETECT
	capAutomatic  = C.SANE_CAP_AUTOMATIC

	constraintRange      = C.SANE_CONSTRAINT_RANGE
	constraintWordList   = C.SANE_CONSTRAINT_WORD_LIST
	constraintStringList = C.SANE_CONSTRAINT_STRING_LIST

	sizeofWord   = C.sizeof_SANE_Word
	sizeofOption = C.sizeof_SANE_Option_Descriptor
	sizeofRange  = C.sizeof_SANE_Range
	sizeofDevice = C.sizeof_SANE_Device
)
