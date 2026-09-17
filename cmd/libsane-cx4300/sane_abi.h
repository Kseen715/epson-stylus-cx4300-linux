/* The part of the SANE 1.0 backend ABI this backend implements.
 *
 * This is a self-contained copy rather than an #include of <sane/sane.h> so
 * that building the backend needs nothing but a C compiler - no sane-backends
 * development package, which is named differently on every distribution the
 * installer supports. The ABI has been frozen since SANE 1.0 (1998), so the
 * layouts below are stable; they are laid out in the same order and with the
 * same types as the upstream header, and must not be reordered.
 */

#ifndef CX4300_SANE_ABI_H
#define CX4300_SANE_ABI_H

typedef unsigned char SANE_Byte;
typedef int SANE_Word;
typedef SANE_Word SANE_Bool;
typedef SANE_Word SANE_Int;
typedef char SANE_Char;
typedef SANE_Char *SANE_String;
typedef const SANE_Char *SANE_String_Const;
typedef void *SANE_Handle;
typedef SANE_Word SANE_Fixed;

#define SANE_FALSE 0
#define SANE_TRUE 1

/* Fixed-point values are 16.16. */
#define SANE_FIXED_SCALE_SHIFT 16

#define SANE_VERSION_CODE(major, minor, build) \
  (((SANE_Word)(major) & 0xff) << 24 | ((SANE_Word)(minor) & 0xff) << 16 | \
   ((SANE_Word)(build) & 0xffff) << 0)

typedef enum
{
  SANE_STATUS_GOOD = 0,
  SANE_STATUS_UNSUPPORTED,
  SANE_STATUS_CANCELLED,
  SANE_STATUS_DEVICE_BUSY,
  SANE_STATUS_INVAL,
  SANE_STATUS_EOF,
  SANE_STATUS_JAMMED,
  SANE_STATUS_NO_DOCS,
  SANE_STATUS_COVER_OPEN,
  SANE_STATUS_IO_ERROR,
  SANE_STATUS_NO_MEM,
  SANE_STATUS_ACCESS_DENIED
}
SANE_Status;

typedef enum
{
  SANE_TYPE_BOOL = 0,
  SANE_TYPE_INT,
  SANE_TYPE_FIXED,
  SANE_TYPE_STRING,
  SANE_TYPE_BUTTON,
  SANE_TYPE_GROUP
}
SANE_Value_Type;

typedef enum
{
  SANE_UNIT_NONE = 0,
  SANE_UNIT_PIXEL,
  SANE_UNIT_BIT,
  SANE_UNIT_MM,
  SANE_UNIT_DPI,
  SANE_UNIT_PERCENT,
  SANE_UNIT_MICROSECOND
}
SANE_Unit;

typedef struct
{
  SANE_String_Const name;
  SANE_String_Const vendor;
  SANE_String_Const model;
  SANE_String_Const type;
}
SANE_Device;

#define SANE_CAP_SOFT_SELECT (1 << 0)
#define SANE_CAP_HARD_SELECT (1 << 1)
#define SANE_CAP_SOFT_DETECT (1 << 2)
#define SANE_CAP_EMULATED (1 << 3)
#define SANE_CAP_AUTOMATIC (1 << 4)
#define SANE_CAP_INACTIVE (1 << 5)
#define SANE_CAP_ADVANCED (1 << 6)

#define SANE_INFO_INEXACT (1 << 0)
#define SANE_INFO_RELOAD_OPTIONS (1 << 1)
#define SANE_INFO_RELOAD_PARAMS (1 << 2)

typedef enum
{
  SANE_CONSTRAINT_NONE = 0,
  SANE_CONSTRAINT_RANGE,
  SANE_CONSTRAINT_WORD_LIST,
  SANE_CONSTRAINT_STRING_LIST
}
SANE_Constraint_Type;

typedef struct
{
  SANE_Word min;
  SANE_Word max;
  SANE_Word quant;
}
SANE_Range;

typedef struct
{
  SANE_String_Const name;
  SANE_String_Const title;
  SANE_String_Const desc;
  SANE_Value_Type type;
  SANE_Unit unit;
  SANE_Int size;
  SANE_Int cap;
  SANE_Constraint_Type constraint_type;
  union
  {
    const SANE_String_Const *string_list;
    const SANE_Word *word_list;
    const SANE_Range *range;
  }
  constraint;
}
SANE_Option_Descriptor;

typedef enum
{
  SANE_ACTION_GET_VALUE = 0,
  SANE_ACTION_SET_VALUE,
  SANE_ACTION_SET_AUTO
}
SANE_Action;

typedef enum
{
  SANE_FRAME_GRAY = 0,
  SANE_FRAME_RGB,
  SANE_FRAME_RED,
  SANE_FRAME_GREEN,
  SANE_FRAME_BLUE
}
SANE_Frame;

typedef struct
{
  SANE_Frame format;
  SANE_Bool last_frame;
  SANE_Int bytes_per_line;
  SANE_Int pixels_per_line;
  SANE_Int lines;
  SANE_Int depth;
}
SANE_Parameters;

#define SANE_MAX_USERNAME_LEN 128
#define SANE_MAX_PASSWORD_LEN 128

typedef void (*SANE_Auth_Callback) (SANE_String_Const resource,
                                    SANE_Char *username,
                                    SANE_Char *password);

#endif /* CX4300_SANE_ABI_H */
