package afp

import "fmt"

// AFP command codes.
const (
	cmdCloseVol        uint8 = 2
	cmdCloseFork       uint8 = 4
	cmdCreateDir       uint8 = 6
	cmdCreateFile      uint8 = 7
	cmdDelete          uint8 = 8
	cmdFlush           uint8 = 10
	cmdFlushFork       uint8 = 11
	cmdGetSrvrParms    uint8 = 16
	cmdGetVolParms     uint8 = 17
	cmdLogin           uint8 = 18
	cmdLoginCont       uint8 = 19
	cmdLogout          uint8 = 20
	cmdMoveAndRename   uint8 = 23
	cmdOpenVol         uint8 = 24
	cmdOpenFork        uint8 = 26
	cmdRename          uint8 = 28
	cmdSetForkParms    uint8 = 31
	cmdGetFileDirParms uint8 = 34
	cmdReadExt         uint8 = 60
	cmdWriteExt        uint8 = 61
	cmdEnumerateExt2   uint8 = 68
)

// AFP result codes (returned in the DSI reply header's error field).
type Result int32

const (
	ResNoErr             Result = 0
	ResAccessDenied      Result = -5000
	ResAuthContinue      Result = -5001
	ResBadUAM            Result = -5002
	ResBadVersNum        Result = -5003
	ResBitmapErr         Result = -5004
	ResDenyConflict      Result = -5006
	ResDirNotEmpty       Result = -5007
	ResDiskFull          Result = -5008
	ResEOF               Result = -5009
	ResFileBusy          Result = -5010
	ResItemNotFound      Result = -5012
	ResLockErr           Result = -5013
	ResMiscErr           Result = -5014
	ResNoServer          Result = -5016
	ResObjectExists      Result = -5017
	ResObjectNotFound    Result = -5018
	ResParamErr          Result = -5019
	ResSessClosed        Result = -5022
	ResUserNotAuth       Result = -5023
	ResCallNotSupported  Result = -5024
	ResObjectTypeErr     Result = -5025
	ResTooManyFilesOpen  Result = -5026
	ResServerGoingDown   Result = -5027
	ResDirNotFound       Result = -5029
	ResVolLocked         Result = -5031
	ResObjectLocked      Result = -5032
	ResPwdExpiredErr     Result = -5042
	ResPwdNeedsChangeErr Result = -5045
)

var resultNames = map[Result]string{
	ResNoErr:             "no error",
	ResAccessDenied:      "access denied",
	ResAuthContinue:      "authentication continues",
	ResBadUAM:            "bad UAM",
	ResBadVersNum:        "bad AFP version",
	ResBitmapErr:         "bitmap error",
	ResDenyConflict:      "deny conflict",
	ResDirNotEmpty:       "directory not empty",
	ResDiskFull:          "disk full",
	ResEOF:               "end of file",
	ResFileBusy:          "file busy",
	ResItemNotFound:      "item not found",
	ResLockErr:           "lock error",
	ResMiscErr:           "miscellaneous error",
	ResNoServer:          "no server",
	ResObjectExists:      "object exists",
	ResObjectNotFound:    "object not found",
	ResParamErr:          "parameter error",
	ResSessClosed:        "session closed",
	ResUserNotAuth:       "user not authenticated",
	ResCallNotSupported:  "call not supported",
	ResObjectTypeErr:     "object type error",
	ResTooManyFilesOpen:  "too many files open",
	ResServerGoingDown:   "server going down",
	ResDirNotFound:       "directory not found",
	ResVolLocked:         "volume locked",
	ResObjectLocked:      "object locked",
	ResPwdExpiredErr:     "password expired",
	ResPwdNeedsChangeErr: "password needs change",
}

// Error is a non-zero AFP result code returned by the server.
type Error struct {
	Op   string
	Code Result
}

func (e *Error) Error() string {
	name, ok := resultNames[e.Code]
	if !ok {
		name = fmt.Sprintf("AFP error %d", e.Code)
	}
	return fmt.Sprintf("afp: %s: %s", e.Op, name)
}

// resultErr converts a result code into an error (nil for ResNoErr).
func resultErr(op string, code Result) error {
	if code == ResNoErr {
		return nil
	}
	return &Error{Op: op, Code: code}
}
