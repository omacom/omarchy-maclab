// smc-power: the whole Mac's power, from the SMC key PSTR, sampled once a
// second through IOKit's AppleSMC.
//
//   smc-power [--seconds N]     N from 1 to 86400, default 60
//
// Prints one JSON line:
//   {"key":"PSTR","seconds":N,"samples":N,"valid":V,"mean_watts":W}
// A sample is valid when the read succeeds and gives a finite value above 0
// and under 1000 W; the mean is over valid samples only. On failure it
// prints {"key":"PSTR","error":CODE} and exits non-zero:
//   2 usage, 3 no_smc, 4 no_pstr, 5 pstr_type, 6 no_valid_samples.
#include <IOKit/IOKitLib.h>
#include <errno.h>
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

enum {
    EXIT_USAGE = 2,
    EXIT_NO_SMC = 3,
    EXIT_NO_PSTR = 4,
    EXIT_PSTR_TYPE = 5,
    EXIT_NO_VALID = 6,
};

// AppleSMC's user client: one struct in, one out, selector 2.
enum { SMC_HANDLE_EVENT = 2, SMC_READ_BYTES = 5, SMC_READ_KEYINFO = 9 };

typedef struct {
    uint8_t major, minor, build, reserved;
    uint16_t release;
} smc_vers;

typedef struct {
    uint16_t version, length;
    uint32_t cpu_limit, gpu_limit, mem_limit;
} smc_plimit;

typedef struct {
    uint32_t size;
    uint32_t type;
    uint8_t attributes;
} smc_keyinfo;

typedef struct {
    uint32_t key;
    smc_vers vers;
    smc_plimit plimit;
    smc_keyinfo info;
    uint8_t result;
    uint8_t status;
    uint8_t data8;
    uint32_t data32;
    uint8_t bytes[32];
} smc_param;

_Static_assert(sizeof(smc_param) == 80, "AppleSMC's parameter struct is 80 bytes");

static uint32_t fourcc(const char *s) {
    return (uint32_t)s[0] << 24 | (uint32_t)s[1] << 16 | (uint32_t)s[2] << 8 | (uint32_t)s[3];
}

static int fail(const char *code, int status) {
    printf("{\"key\":\"PSTR\",\"error\":\"%s\"}\n", code);
    return status;
}

static kern_return_t smc_call(io_connect_t conn, smc_param *in, smc_param *out) {
    size_t size = sizeof(*out);
    memset(out, 0, sizeof(*out));
    return IOConnectCallStructMethod(conn, SMC_HANDLE_EVENT, in, sizeof(*in), out, &size);
}

static int key_info(io_connect_t conn, uint32_t key, smc_keyinfo *info) {
    smc_param in = {0}, out;
    in.key = key;
    in.data8 = SMC_READ_KEYINFO;
    if (smc_call(conn, &in, &out) != kIOReturnSuccess || out.result != 0) return -1;
    *info = out.info;
    return 0;
}

static int read_float(io_connect_t conn, uint32_t key, const smc_keyinfo *info, double *watts) {
    smc_param in = {0}, out;
    in.key = key;
    in.info.size = info->size;
    in.data8 = SMC_READ_BYTES;
    if (smc_call(conn, &in, &out) != kIOReturnSuccess || out.result != 0) return -1;
    float f;
    memcpy(&f, out.bytes, sizeof(f));
    *watts = f;
    return 0;
}

static void sleep_until(const struct timespec *t) {
    struct timespec now, d;
    for (;;) {
        clock_gettime(CLOCK_MONOTONIC, &now);
        d.tv_sec = t->tv_sec - now.tv_sec;
        d.tv_nsec = t->tv_nsec - now.tv_nsec;
        if (d.tv_nsec < 0) {
            d.tv_sec--;
            d.tv_nsec += 1000000000L;
        }
        if (d.tv_sec < 0) return;
        if (nanosleep(&d, NULL) == 0 || errno != EINTR) return;
    }
}

int main(int argc, char **argv) {
    long seconds = 60;
    for (int i = 1; i < argc; i++) {
        if (strcmp(argv[i], "--seconds") == 0 && i + 1 < argc) {
            char *end;
            errno = 0;
            seconds = strtol(argv[++i], &end, 10);
            if (errno || *end || seconds < 1 || seconds > 86400) return fail("usage", EXIT_USAGE);
        } else {
            fprintf(stderr, "usage: smc-power [--seconds N]\n");
            return fail("usage", EXIT_USAGE);
        }
    }

    io_service_t smc = IOServiceGetMatchingService(kIOMainPortDefault, IOServiceMatching("AppleSMC"));
    if (!smc) return fail("no_smc", EXIT_NO_SMC);
    io_connect_t conn;
    kern_return_t kr = IOServiceOpen(smc, mach_task_self(), 0, &conn);
    IOObjectRelease(smc);
    if (kr != kIOReturnSuccess) return fail("no_smc", EXIT_NO_SMC);

    uint32_t key = fourcc("PSTR");
    smc_keyinfo info;
    if (key_info(conn, key, &info) != 0) {
        IOServiceClose(conn);
        return fail("no_pstr", EXIT_NO_PSTR);
    }
    if (info.type != fourcc("flt ") || info.size != 4) {
        IOServiceClose(conn);
        return fail("pstr_type", EXIT_PSTR_TYPE);
    }

    long valid = 0;
    double sum = 0;
    struct timespec next;
    clock_gettime(CLOCK_MONOTONIC, &next);
    for (long n = 0; n < seconds; n++) {
        next.tv_sec++;
        sleep_until(&next);
        double w;
        if (read_float(conn, key, &info, &w) == 0 && isfinite(w) && w > 0 && w < 1000) {
            sum += w;
            valid++;
        }
    }
    IOServiceClose(conn);
    if (valid == 0) return fail("no_valid_samples", EXIT_NO_VALID);
    printf("{\"key\":\"PSTR\",\"seconds\":%ld,\"samples\":%ld,\"valid\":%ld,\"mean_watts\":%.3f}\n",
           seconds, seconds, valid, sum / valid);
    return 0;
}
