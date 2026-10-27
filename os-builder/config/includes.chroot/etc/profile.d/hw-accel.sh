# Detect GPU vendor at boot and set VA-API / VDPAU drivers for VDI offload
if [ -d /dev/dri ]; then
    if lspci -nn | grep -q -E "VGA.*\[10de:"; then
        export LIBVA_DRIVER_NAME=nvidia
        export VDPAU_DRIVER=nvidia
        export NVD_BACKEND=direct
    else
        # Intel 12th, 13th, 14th Gen (Alder/Raptor/Meteor Lake)
        export LIBVA_DRIVER_NAME=iHD
        export GST_VAAPI_ALL_DRIVERS=1
    fi
fi
