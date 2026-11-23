// mlx-bench: an fp16 matrix multiply on the GPU through MLX, in TFLOPS.
//
//   mlx-bench [--n N] [--warmup N] [--iterations N]
//
// Multiplies two N x N fp16 matrices (default 4096) on the GPU stream,
// evaluating and synchronising after each product so the clock covers the
// GPU's work. Prints one JSON line; with no Metal GPU it prints
// {"error":"no_gpu"} and exits 1 (2 for usage).
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <exception>
#include <string>

#include "mlx/backend/metal/metal.h"
#include "mlx/mlx.h"

namespace mx = mlx::core;

static int fail(const char *code, int status) {
    std::printf("{\"error\":\"%s\"}\n", code);
    return status;
}

static bool number(const char *s, long least, long most, long *out) {
    char *end;
    long n = std::strtol(s, &end, 10);
    if (*s == '\0' || *end != '\0' || n < least || n > most) return false;
    *out = n;
    return true;
}

int main(int argc, char **argv) {
    long n = 4096, warmup = 5, iterations = 50;
    for (int i = 1; i < argc; i++) {
        bool ok = i + 1 < argc;
        if (ok && std::strcmp(argv[i], "--n") == 0) {
            ok = number(argv[++i], 256, 16384, &n);
        } else if (ok && std::strcmp(argv[i], "--warmup") == 0) {
            ok = number(argv[++i], 0, 10000, &warmup);
        } else if (ok && std::strcmp(argv[i], "--iterations") == 0) {
            ok = number(argv[++i], 1, 100000, &iterations);
        } else {
            ok = false;
        }
        if (!ok) {
            std::fprintf(stderr, "usage: mlx-bench [--n N] [--warmup N] [--iterations N]\n");
            return fail("usage", 2);
        }
    }
    if (!mx::metal::is_available()) return fail("no_gpu", 1);

    try {
        mx::Device gpu(mx::Device::gpu);
        mx::set_default_device(gpu);
        mx::Stream s = mx::default_stream(gpu);
        int dim = static_cast<int>(n);
        auto a = mx::random::normal({dim, dim}, mx::float16, std::nullopt, s);
        auto b = mx::random::normal({dim, dim}, mx::float16, std::nullopt, s);
        mx::eval({a, b});
        mx::synchronize(s);

        auto step = [&] {
            auto c = mx::matmul(a, b, s);
            mx::eval(c);
            mx::synchronize(s);
        };
        for (long i = 0; i < warmup; i++) step();
        auto start = std::chrono::steady_clock::now();
        for (long i = 0; i < iterations; i++) step();
        double seconds = std::chrono::duration<double>(std::chrono::steady_clock::now() - start).count();
        double flops = 2.0 * static_cast<double>(n) * static_cast<double>(n) * static_cast<double>(n) * static_cast<double>(iterations);
        std::printf(
            "{\"device\":\"gpu\",\"dtype\":\"float16\",\"n\":%ld,\"warmup\":%ld,\"iterations\":%ld,"
            "\"seconds\":%.6f,\"tflops\":%.4f,\"mlx\":\"%s\"}\n",
            n, warmup, iterations, seconds, flops / seconds / 1e12, mx::version());
    } catch (const std::exception &) {
        return fail("gpu_run", 1);
    }
    return 0;
}
