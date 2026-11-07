.PHONY: all clean test
all: hadron-splash

CFLAGS_COMMON = -std=c99 -Wall -Wextra -Wpedantic

hadron-splash: main.c agent.c agent.h font.h
	@gcc -Os $(CFLAGS_COMMON) \
		-fno-asynchronous-unwind-tables -fno-unwind-tables -fno-stack-protector \
		-ffunction-sections -fdata-sections -fno-plt -fno-pie -no-pie -flto \
		-Wl,--gc-sections -Wl,--build-id=none -Wl,-z,noseparate-code -Wl,-z,norelro \
		-Wl,--hash-style=gnu -s -o $@ main.c agent.c -lm $(LDFLAGS)

# The password agent protocol on its own: parsing, the title, the retry rule
# and the reply on a real socket.
test_agent: test_agent.c agent.c agent.h
	@gcc -O1 -g $(CFLAGS_COMMON) -o $@ test_agent.c agent.c

test: test_agent
	@./test_agent

clean:
	@rm -f hadron-splash test_agent *.o
