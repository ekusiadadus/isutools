#include <errno.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

enum {
    control_fd = 3,
    ready_fd = 4,
    chunk_bytes = 8 * 1024 * 1024,
};

static uint64_t env_limit(const char *name) {
    const char *value = getenv(name);
    if (value == NULL || *value == '\0') {
        return 0;
    }
    return strtoull(value, NULL, 10);
}

int main(void) {
    uint64_t memory_max = env_limit("ISUTOOLS_PPROF_WORKER_MEMORY_MAX");
    uint64_t address_max = env_limit("ISUTOOLS_PPROF_WORKER_AS_MAX");
    if (memory_max == 0 || address_max == 0) {
        return 4;
    }
    if (dprintf(ready_fd,
                "{\"pid\":%ld,\"isolation\":{\"mode\":\"linux-cgroup-v2\","
                "\"bootstrap\":\"cgroupfd-sigstop\",\"memory_max_bytes\":%llu,"
                "\"address_space_max_bytes\":%llu,\"hard_limit_verified\":false,"
                "\"stopped_verified\":false}}\n",
                (long)getpid(), (unsigned long long)memory_max,
                (unsigned long long)address_max) < 0) {
        return 4;
    }
    close(ready_fd);
    if (raise(SIGSTOP) != 0) {
        return 4;
    }
    char command[6];
    ssize_t got = read(control_fd, command, sizeof(command));
    close(control_fd);
    if (got != 6 || memcmp(command, "START\n", 6) != 0) {
        return 4;
    }
    for (;;) {
        unsigned char *chunk = mmap(NULL, chunk_bytes, PROT_READ | PROT_WRITE,
                                    MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
        if (chunk == MAP_FAILED) {
            fprintf(stderr, "mmap failed before cgroup OOM: %s\n", strerror(errno));
            return 4;
        }
        for (size_t offset = 0; offset < chunk_bytes; offset += 4096) {
            chunk[offset] = 1;
        }
    }
}
