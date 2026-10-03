CC ?= gcc
CFLAGS ?= -O2 -Wall -Wextra -Iinclude -pthread
LDFLAGS ?= -pthread -ldl -lutil

BUILD_DIR = build
SRC_DIR = src

OBJS = $(BUILD_DIR)/pty_session.o $(BUILD_DIR)/vnc_server.o $(BUILD_DIR)/agent_file_cmd.o $(BUILD_DIR)/stream_transport.o

all: linux

linux: $(BUILD_DIR) bin/mar4uder_agent bin/mar4uder_client

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR) bin

$(BUILD_DIR)/pty_session.o: $(SRC_DIR)/pty_session.c include/pty_session.h
	$(CC) $(CFLAGS) -c $< -o $@

$(BUILD_DIR)/vnc_server.o: $(SRC_DIR)/vnc_server.c include/vnc_server.h
	$(CC) $(CFLAGS) -c $< -o $@

$(BUILD_DIR)/agent_file_cmd.o: $(SRC_DIR)/agent_file_cmd.c include/agent_file_cmd.h include/protocol.h
	$(CC) $(CFLAGS) -c $< -o $@

$(BUILD_DIR)/stream_transport.o: $(SRC_DIR)/stream_transport.c include/stream_transport.h include/protocol.h
	$(CC) $(CFLAGS) -c $< -o $@

bin/mar4uder_agent: $(SRC_DIR)/agent_main.c $(OBJS) include/protocol.h
	$(CC) $(CFLAGS) $< $(OBJS) -o $@ $(LDFLAGS)

bin/mar4uder_client: $(SRC_DIR)/client_main.c
	$(CC) -O2 -Iinclude $< -o $@ -static

clean:
	rm -rf $(BUILD_DIR) bin

.PHONY: all linux clean
