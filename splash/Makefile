.PHONY: all clean test
all: isotope-splash

CFLAGS_COMMON = -std=c99 -Wall -Wextra -Wpedantic

# -O2, not -Os: the light is arithmetic per pixel, and -O2 draws a frame in two
# thirds of the time for 10 KB more.
isotope-splash: main.c agent.c agent.h masks.h
	@gcc -O2 $(CFLAGS_COMMON) \
		-fno-asynchronous-unwind-tables -fno-unwind-tables -fno-stack-protector \
		-ffunction-sections -fdata-sections -fno-plt -fno-pie -no-pie -flto \
		-Wl,--gc-sections -Wl,--build-id=none -Wl,-z,noseparate-code -Wl,-z,norelro \
		-Wl,--hash-style=gnu -s -o $@ main.c agent.c -lm $(LDFLAGS)

# The password agent protocol on its own: parsing, the title, the retry rule
# and the reply on a real socket.
test_agent: test_agent.c agent.c agent.h
	@gcc -O1 -g $(CFLAGS_COMMON) -o $@ test_agent.c agent.c

test: test_agent isotope-splash
	@./test_agent
	@# A settled frame shows the mark: the palette's brand yellow #e4de48.
	@./isotope-splash --render=test-frame.ppm --at=3 && \
		LC_ALL=C grep -q "$$(printf '\344\336\110')" test-frame.ppm && \
		echo "splash frame has the mark" || { echo "splash frame has no mark"; exit 1; }
	@rm -f test-frame.ppm

clean:
	@rm -f isotope-splash test_agent test-frame.ppm *.o
