/* The SANE entry points under their plain names.
 *
 * SANE's dll backend looks a backend's functions up as sane_<backend>_<op>,
 * which is what the Go side exports. These wrappers add the unprefixed
 * sane_<op> names as well, exactly as the backends shipped with sane-backends
 * do, so that this library also works when a frontend loads it directly - as
 * libsane.so.1, or through SANE_CONFIG/LD_PRELOAD - rather than through
 * the dll backend.
 *
 * The casts are for const only: cgo drops const qualifiers when it generates
 * the declarations in _cgo_export.h, so the types here are the ones SANE's
 * header declares and the ones underneath are identical.
 */

#include "_cgo_export.h"

SANE_Status
sane_init (SANE_Int *version_code, SANE_Auth_Callback authorize)
{
  return sane_cx4300_init (version_code, authorize);
}

void
sane_exit (void)
{
  sane_cx4300_exit ();
}

SANE_Status
sane_get_devices (const SANE_Device ***device_list, SANE_Bool local_only)
{
  return sane_cx4300_get_devices ((SANE_Device ***) device_list, local_only);
}

SANE_Status
sane_open (SANE_String_Const devicename, SANE_Handle *handle)
{
  return sane_cx4300_open ((SANE_String_Const) devicename, handle);
}

void
sane_close (SANE_Handle handle)
{
  sane_cx4300_close (handle);
}

const SANE_Option_Descriptor *
sane_get_option_descriptor (SANE_Handle handle, SANE_Int option)
{
  return sane_cx4300_get_option_descriptor (handle, option);
}

SANE_Status
sane_control_option (SANE_Handle handle, SANE_Int option, SANE_Action action,
                     void *value, SANE_Int *info)
{
  return sane_cx4300_control_option (handle, option, action, value, info);
}

SANE_Status
sane_get_parameters (SANE_Handle handle, SANE_Parameters *params)
{
  return sane_cx4300_get_parameters (handle, params);
}

SANE_Status
sane_start (SANE_Handle handle)
{
  return sane_cx4300_start (handle);
}

SANE_Status
sane_read (SANE_Handle handle, SANE_Byte *data, SANE_Int max_length,
           SANE_Int *length)
{
  return sane_cx4300_read (handle, data, max_length, length);
}

void
sane_cancel (SANE_Handle handle)
{
  sane_cx4300_cancel (handle);
}

SANE_Status
sane_set_io_mode (SANE_Handle handle, SANE_Bool non_blocking)
{
  return sane_cx4300_set_io_mode (handle, non_blocking);
}

SANE_Status
sane_get_select_fd (SANE_Handle handle, SANE_Int *fd)
{
  return sane_cx4300_get_select_fd (handle, fd);
}
