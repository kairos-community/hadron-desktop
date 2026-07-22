# Build either Hadron desktop variant, the opt-in agent overlay, and bootable
# installer ISOs.
#
#   make image        # build the desktop image (Kairos init layer folded in)
#   make iso          # build the image + the installer ISO
#   make              # both (image, then iso)
#   make DESKTOP=i3 image               # XLibre + i3 variant
#   make images                          # build both variant images
#   make agent-image                     # build the i3-based agent overlay
#   make agent-iso                       # build the agent ISO
#   make clean                           # remove build artifacts
#
# Knobs (override on the command line):
#   make DESKTOP=i3 GPU=full FIRMWARE=true # XLibre/i3 + hardware support
#   make IMAGE=sway-desktop:hw             # change the image tag
#   make BASE_IMAGE=ghcr.io/...:vX        # pin a different Hadron base
#   make VERSION=v1.2.3                   # stamp a Kairos version

DESKTOP      ?= sway
IMAGE        ?= $(DESKTOP)-desktop:dev
BASE_IMAGE   ?= ghcr.io/kairos-io/hadron:main
AURORA_IMAGE ?= quay.io/kairos/auroraboot:v0.21.0-alpha.4
AGENT_BASE_IMAGE ?= i3-desktop:dev
AGENT_IMAGE      ?= agent-desktop:dev
AGENT_WORK       := build/agent-desktop
AGENT_ISO_DIR    := $(AGENT_WORK)/iso

VALID_DESKTOPS := sway i3
ifneq ($(words $(DESKTOP)),1)
$(error unsupported DESKTOP '$(DESKTOP)'; choose exactly one of: $(VALID_DESKTOPS))
endif
ifeq ($(filter $(DESKTOP),$(VALID_DESKTOPS)),)
$(error unsupported DESKTOP '$(DESKTOP)'; choose one of: $(VALID_DESKTOPS))
endif

WORK    := build/$(DESKTOP)-desktop
ISO_DIR := $(WORK)/iso

# Optional build args, only passed when set (otherwise the Dockerfile defaults
# apply: GPU=vm, FIRMWARE=false, VERSION=v0.0.0).
BUILD_ARGS := --build-arg BASE_IMAGE=$(BASE_IMAGE) --build-arg DESKTOP=$(DESKTOP)
ifdef GPU
BUILD_ARGS += --build-arg GPU=$(GPU)
endif
ifdef FIRMWARE
BUILD_ARGS += --build-arg FIRMWARE=$(FIRMWARE)
endif
ifdef VERSION
BUILD_ARGS += --build-arg VERSION=$(VERSION)
endif

export DOCKER_BUILDKIT := 1

.PHONY: all image images iso agent-image agent-iso vm vm-install clean

all: iso

# The desktop image, with the Kairos init layer folded in as the final stage
# (build `--target default` for the bare desktop image without it).
# --no-cache-filter on hadron-splash and kairos: the kairos stage runs
# kairos-init's `dracut -f`, which bakes the splash into /boot/initrd. BuildKit
# happily caches that step even when splash/main.c changed, silently shipping a
# stale initramfs splash. Busting both stages keeps the initramfs honest.
image:
	docker build $(BUILD_ARGS) --no-cache-filter hadron-splash,kairos -t $(IMAGE) .

images:
	$(MAKE) DESKTOP=sway image
	$(MAKE) DESKTOP=i3 image

agent-image:
	$(MAKE) DESKTOP=i3 IMAGE=$(AGENT_BASE_IMAGE) $(if $(VERSION),VERSION=$(VERSION),) image
	docker build -f Dockerfile.agent --target agent \
	  --build-arg BASE_IMAGE=$(AGENT_BASE_IMAGE) \
	  $(if $(VERSION),--build-arg VERSION=$(VERSION),) \
	  -t $(AGENT_IMAGE) .

agent-iso: agent-image
	mkdir -p $(AGENT_ISO_DIR)
	rm -f $(AGENT_ISO_DIR)/*.iso
	docker run --rm --privileged \
	  -v /var/run/docker.sock:/var/run/docker.sock \
	  -v $(CURDIR)/$(AGENT_ISO_DIR):/output \
	  $(AURORA_IMAGE) build-iso --output /output/ docker:$(AGENT_IMAGE)
	@echo "ISO: $$(ls -t $(AGENT_ISO_DIR)/*.iso | head -1)"

# Build the installer ISO with AuroraBoot straight from the image (it reads the
# local image over the Docker socket).
iso: image
	mkdir -p $(ISO_DIR)
	rm -f $(ISO_DIR)/*.iso
	docker run --rm --privileged \
	  -v /var/run/docker.sock:/var/run/docker.sock \
	  -v $(CURDIR)/$(ISO_DIR):/output \
	  $(AURORA_IMAGE) build-iso --output /output/ docker:$(IMAGE)
	@echo "ISO: $$(ls -t $(ISO_DIR)/*.iso | head -1)"

# Run the image in QEMU with the correct flags (UEFI + virtio-gpu, NOT the
# default VGA which renders the boot console as garbled static). See tools/vm.sh
# for env knobs (NOVNC=1, FRESH=1, MEM, VNC, ...).
vm-install:           ## fresh disk, boot the newest installer ISO
	tools/vm.sh install
vm:                   ## boot the already-installed disk
	tools/vm.sh run

clean:
	rm -rf $(WORK) $(AGENT_WORK)
