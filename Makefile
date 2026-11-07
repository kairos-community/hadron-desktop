# Build either Hadron desktop variant, the opt-in agent overlay, and bootable
# installer ISOs.
#
#   make image        # build the desktop image (Kairos init layer folded in)
#   make iso          # build the image + the installer ISO
#   make              # both (image, then iso)
#   make DESKTOP=i3 image               # XLibre + i3 variant
#   make DESKTOP=cosmic image           # COSMIC (stacking, for newcomers)
#   make DESKTOP=niri image             # niri + Noctalia (scrolling tiling)
#   make images                          # build both variant images
#   make agent-image                     # build the i3-based agent overlay
#   make agent-iso                       # build the agent ISO
#   make LLVM_OUT=<ref> llvm-image       # build only LLVM, for LLVM_IMAGE=<ref>
#   make STAGE=cosmic-shell STAGE_OUT=<ref> stage-image  # one prebuilt half of COSMIC
#   make uki-keys                        # Secure Boot test keys for a UKI ISO
#   make uki-iso                         # build a Trusted Boot (UKI) ISO
#   make UKI_FROM=<ref> uki-iso          # the same, from a published image
#   make clean                           # remove build artifacts
#
# Knobs (override on the command line):
#   make DESKTOP=i3 GPU=full FIRMWARE=true # XLibre/i3 + hardware support
#                                          (what a release publishes as
#                                          <flavor>-<tag>; GPU=vm, the default
#                                          here, is <flavor>-vm-<tag>)
#   make IMAGE=sway-desktop:hw             # change the image tag
#   make BASE_IMAGE=ghcr.io/...:vX        # pin a different Hadron base
#   make VERSION=v1.2.3                   # stamp a Kairos version
#   make CACHE_REF=ghcr.io/o/r:cache      # reuse a registry build cache (CI)

DESKTOP      ?= sway
IMAGE        ?= $(DESKTOP)-desktop:dev
# Hadron release to build on. scripts/bump-hadron.sh rewrites this line together
# with the HADRON_VERSION defaults in the Dockerfiles.
HADRON_VERSION ?= v0.6.4
BASE_IMAGE   ?= ghcr.io/kairos-io/hadron:$(HADRON_VERSION)
AURORA_IMAGE ?= quay.io/kairos/auroraboot:v0.29.0
AGENT_BASE_IMAGE ?= i3-desktop:dev
AGENT_IMAGE      ?= agent-desktop:dev
AGENT_WORK       := build/agent-desktop
AGENT_ISO_DIR    := $(AGENT_WORK)/iso

VALID_DESKTOPS := sway i3 cosmic niri
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
BUILD_ARGS := --build-arg HADRON_VERSION=$(HADRON_VERSION) --build-arg BASE_IMAGE=$(BASE_IMAGE) --build-arg DESKTOP=$(DESKTOP)
ifdef GPU
BUILD_ARGS += --build-arg GPU=$(GPU)
endif
ifdef FIRMWARE
BUILD_ARGS += --build-arg FIRMWARE=$(FIRMWARE)
endif
ifdef VERSION
BUILD_ARGS += --build-arg VERSION=$(VERSION)
endif

# Registry build cache, for CI. A fresh runner rebuilds every stage from source
# (hours for LLVM, Mesa, COSMIC), so CI points CACHE_REF at a registry ref and
# each image reads <CACHE_REF>-<name>; CACHE_PUSH=true also writes it back.
# mode=max exports every stage, not only the final image's layers, which is
# what lets a later build skip the long stages. The push is best effort: a
# cache that fails to upload must not fail a build that already succeeded.
# Exporting needs the containerd image store when using the default builder.
CACHE_REF  ?=
CACHE_PUSH ?=
comma := ,
cache_from = --cache-from type=registry$(comma)ref=$(CACHE_REF)-$(1)
cache_to   = --cache-to type=registry$(comma)ref=$(CACHE_REF)-$(1)$(comma)mode=max$(comma)ignore-error=true
cache_args = $(if $(CACHE_REF),$(call cache_from,$(1)) $(if $(filter true,$(CACHE_PUSH)),$(call cache_to,$(1))))
# The two image lines build a different Mesa (and every stage built on it), so
# each keeps its own cache: sharing one, each release's second writer would
# replace the first one's stages. GPU=vm keeps the plain name it has always
# had, which pull requests (agent-gates.yml) read.
CACHE_NAME = $(DESKTOP)$(if $(filter full,$(GPU)),-full)

# Prebuilt LLVM, for CI. Every build uses the llvm stage (Mesa's llvmpipe links
# libLLVM, and COSMIC and niri also need libclang), and it compiles LLVM and
# clang for about three hours, so a local build without LLVM_IMAGE takes that
# much longer the first time. LLVM_IMAGE names an image built from the
# llvm-dist target (`make LLVM_OUT=<ref> llvm-image`), and the build uses it in
# place of the llvm stage. The tag from `make print-llvm-tag` is a hash of the
# Hadron release and the llvm stage text, so a stale image is never used: any
# change to either gives a new tag. Release builds publish it as
# ghcr.io/kairos-community/hadron-desktop:<print-llvm-tag>, which a local
# build can use too.
LLVM_IMAGE ?=
ifdef LLVM_IMAGE
BUILD_ARGS += --build-context llvm=docker-image://$(LLVM_IMAGE)
endif

# Prebuilt COSMIC, for CI, the same way. The cosmic stage is the cosmic-shell
# and cosmic-apps stages, about two and a half hours each, which together ran
# past the six-hour limit of one CI job. COSMIC_SHELL_IMAGE and
# COSMIC_APPS_IMAGE name images built from their -dist targets
# (`make STAGE=cosmic-shell STAGE_OUT=<ref> stage-image`), tagged with
# `make STAGE=cosmic-shell print-stage-key`. That key hashes every stage the
# half is built from, the files they copy and the build args they use
# (tools/stage-key.py), so an image is reused until one of those changes.
COSMIC_SHELL_IMAGE ?=
COSMIC_APPS_IMAGE ?=
ifdef COSMIC_SHELL_IMAGE
BUILD_ARGS += --build-context cosmic-shell=docker-image://$(COSMIC_SHELL_IMAGE)
endif
ifdef COSMIC_APPS_IMAGE
BUILD_ARGS += --build-context cosmic-apps=docker-image://$(COSMIC_APPS_IMAGE)
endif
STAGE ?=
STAGE_OUT ?=

export DOCKER_BUILDKIT := 1

.PHONY: all image images iso uki-keys uki-image uki-iso agent-image agent-iso llvm-image stage-image vm vm-install check-boot-ux print-aurora-image print-base-image print-llvm-tag print-stage-key clean

all: iso

# The desktop image, with the Kairos init layer folded in as the final stage
# (build `--target default` for the bare desktop image without it).
# --no-cache-filter on kairos: that stage runs kairos-init's `dracut -f`, which
# bakes the splash branding and everything else into /boot/initrd, and it
# follows the moving kairos-init:master tag. BuildKit happily caches the step
# anyway, silently shipping a stale initramfs. Busting it keeps the initramfs
# honest; it is the cheap stage at the very end.
image:
	docker build $(BUILD_ARGS) $(call cache_args,$(CACHE_NAME)) --no-cache-filter kairos -t $(IMAGE) .

images:
	$(MAKE) DESKTOP=sway image
	$(MAKE) DESKTOP=i3 image

# The agent is a VM appliance: always the vm line (no hardware GPU drivers, no
# firmware), whatever GPU or FIRMWARE the command line sets.
agent-image:
	$(MAKE) DESKTOP=i3 GPU=vm FIRMWARE=false IMAGE=$(AGENT_BASE_IMAGE) $(if $(VERSION),VERSION=$(VERSION),) image
	docker build -f Dockerfile.agent --target agent $(call cache_args,agent) \
	  --build-arg BASE_IMAGE=$(AGENT_BASE_IMAGE) \
	  --build-arg HADRON_VERSION=$(HADRON_VERSION) \
	  $(if $(VERSION),--build-arg VERSION=$(VERSION),) \
	  -t $(AGENT_IMAGE) .

agent-iso: agent-image
	AURORA_IMAGE=$(AURORA_IMAGE) auroraboot/build.sh $(AGENT_IMAGE) $(AGENT_ISO_DIR)

# Build the installer ISO with AuroraBoot via auroraboot/build.sh, which stages
# an --overlay-iso dir carrying our own live GRUB menu (branded, themed, quiet
# cmdline) so AuroraBoot keeps ours instead of writing its default Kairos menu.
# build.sh also creates the output dir and clears stale ISOs, which these
# targets used to do inline.
llvm-image:
	@[ -n "$(LLVM_OUT)" ] || { echo "set LLVM_OUT to the image reference to build" >&2; exit 1; }
	docker build --build-arg HADRON_VERSION=$(HADRON_VERSION) --target llvm-dist -t $(LLVM_OUT) .

# llvm-<version>-<key>: the key hashes the Hadron release (the toolchain the
# stage builds on) and the stage's own text, from `FROM toolchain AS llvm` up
# to the next FROM.
print-llvm-tag:
	@printf 'llvm-%s-%s\n' \
	  "$$(awk -F= '/^ARG LLVM_VERSION=/{print $$2; exit}' Dockerfile)" \
	  "$$({ echo '$(HADRON_VERSION)'; awk '/^FROM toolchain AS llvm$$/{p=1; print; next} p && /^FROM /{exit} p' Dockerfile; } | sha256sum | cut -c1-12)"

# STAGE is cosmic-shell or cosmic-apps: the stages with a <STAGE>-dist target.
stage-image:
	@[ -n "$(STAGE)" ] && [ -n "$(STAGE_OUT)" ] || { echo "set STAGE and STAGE_OUT" >&2; exit 1; }
	docker build $(BUILD_ARGS) --target $(STAGE)-dist -t $(STAGE_OUT) .

# <STAGE>-<key>; the key takes the same build args as `make image`.
print-stage-key:
	@[ -n "$(STAGE)" ] || { echo "set STAGE" >&2; exit 1; }
	@python3 tools/stage-key.py \
	  --arg HADRON_VERSION=$(HADRON_VERSION) --arg BASE_IMAGE=$(BASE_IMAGE) --arg DESKTOP=$(DESKTOP) \
	  $(if $(GPU),--arg GPU=$(GPU)) $(if $(FIRMWARE),--arg FIRMWARE=$(FIRMWARE)) $(if $(VERSION),--arg VERSION=$(VERSION)) \
	  $(STAGE)

iso: image
	AURORA_IMAGE=$(AURORA_IMAGE) auroraboot/build.sh $(IMAGE) $(ISO_DIR)

# Trusted Boot (UKI) edition: Secure Boot, measured boot and disk encryption
# sealed to the TPM. See docs/superpowers/specs/2026-11-07-trusted-boot-edition-design.md.
#
# KEYS_DIR holds the Secure Boot keys. `make uki-keys` writes test keys there;
# a machine installed with them boots only UKIs signed with them, so keep them.
# The image is built on hadron-trusted, the Hadron base without dracut (a UKI
# carries the whole image as its initramfs). UKI_FROM skips the desktop build:
# it re-runs only the kairos stage, with TRUSTED_BOOT=true, on a published
# image such as ghcr.io/kairos-community/hadron-desktop:sway-vm-v0.0.8. Changes
# to rootfs/ in this tree are then not in the ISO, only the kairos stage's.
KEYS_DIR  ?= build/uki-keys
UKI_IMAGE ?= $(DESKTOP)-desktop:uki
UKI_FROM  ?=
UKI_ISO_DIR := $(WORK)/uki-iso
TRUSTED_BASE_IMAGE ?= ghcr.io/kairos-io/hadron-trusted:$(HADRON_VERSION)

uki-keys:
	@[ ! -e $(KEYS_DIR)/db.key ] || { echo "$(KEYS_DIR) already holds keys; machines enrolled with them need them" >&2; exit 1; }
	mkdir -p $(KEYS_DIR)
	docker run --rm -u $$(id -u):$$(id -g) -v $(abspath $(KEYS_DIR)):/keys $(AURORA_IMAGE) \
	  genkey -e 365 --output /keys "Hadron Desktop local"

uki-image:
ifdef UKI_FROM
	docker build $(BUILD_ARGS) --build-arg TRUSTED_BOOT=true --build-arg GPU=$(or $(GPU),vm) \
	  --build-context default=docker-image://$(UKI_FROM) --no-cache-filter kairos --target kairos -t $(UKI_IMAGE) .
else
	docker build $(BUILD_ARGS) --build-arg TRUSTED_BOOT=true --build-arg BASE_IMAGE=$(TRUSTED_BASE_IMAGE) \
	  $(call cache_args,$(CACHE_NAME)) --no-cache-filter kairos -t $(UKI_IMAGE) .
endif

uki-iso: uki-image
	AURORA_IMAGE=$(AURORA_IMAGE) auroraboot/build-uki.sh $(UKI_IMAGE) $(UKI_ISO_DIR) $(KEYS_DIR)

# Run the image in QEMU with the correct flags (UEFI + virtio-gpu, NOT the
# default VGA which renders the boot console as garbled static). See tools/vm.sh
# for env knobs (NOVNC=1, FRESH=1, MEM, VNC, ...).
vm-install:           ## fresh disk, boot the newest installer ISO
	tools/vm.sh install
vm:                   ## boot the already-installed disk
	tools/vm.sh run

# Boot-UX assertions (splash, initramfs, branding, ISO overlay), the update
# tray wiring (test/update/check-tray-wiring.sh) and the live-only default
# user (test/accounting/check-default-user.sh) for every variant image that
# is already built locally. Deliberately NOT a dependency of `all`:
# the checks inspect a built image, so wiring them into a plain `make` would
# either fail before the image exists or force a rebuild. Run them after
# building: `make image && make check-boot-ux`.
#
# Each variant is guarded on `docker image inspect` so having built only one
# variant checks only that one instead of erroring. The subtitle passed here is
# the assertion, not a lookup -- note the agent variant runs i3 and so is
# expected to say "i3 · xlibre · kairos", not "agent".
check-boot-ux:
	@ran=0; fail=0; \
	for spec in "sway-desktop:dev|sway · wayland · kairos" \
	            "i3-desktop:dev|i3 · xlibre · kairos" \
	            "cosmic-desktop:dev|cosmic · wayland · kairos" \
	            "niri-desktop:dev|niri · wayland · kairos" \
	            "agent-desktop:dev|i3 · xlibre · kairos"; do \
	  img="$${spec%%|*}"; subtitle="$${spec#*|}"; \
	  if docker image inspect "$$img" >/dev/null 2>&1; then \
	    echo "==> boot-ux: $$img ($$subtitle)"; \
	    ran=1; \
	    test/boot-ux/run.sh "$$img" "$$subtitle" || fail=1; \
	    test/update/check-tray-wiring.sh "$$img" "$${img%%-desktop:*}" || fail=1; \
	    test/accounting/check-default-user.sh "$$img" || fail=1; \
	  else \
	    echo "==> boot-ux: skipping $$img (not built)"; \
	  fi; \
	done; \
	if [ "$$ran" -eq 0 ]; then \
	  echo "check-boot-ux: no variant image built; run 'make image' first" >&2; exit 1; \
	fi; \
	exit $$fail

# The resolved base reference, for CI steps that pull it before the build.
print-aurora-image:
	@echo $(AURORA_IMAGE)

print-base-image:
	@echo $(BASE_IMAGE)

clean:
	rm -rf $(WORK) $(AGENT_WORK)
