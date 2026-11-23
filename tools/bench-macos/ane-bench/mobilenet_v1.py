#!/usr/bin/env python3
"""Write MobileNet v1 (1.0, 224, 1000 classes, batch 1, ReLU, batch
normalisation folded in) as a Core ML neural network with fp16 weights:

  mobilenet_v1.py OUT.mlmodel

The weights are made here from a fixed seed, the same bytes every time: the
model is timed, never asked what a picture shows. Core ML's file is a
protocol buffer (Apple's coremltools, mlmodel/format), written here field by
field.
"""
import struct
import sys

SPEC_VERSION = 4
FLOAT32 = 65568
CLASSES = 1000
# A depthwise block's stride and output channels.
BLOCKS = [(1, 64), (2, 128), (1, 128), (2, 256), (1, 256), (2, 512), (1, 512), (1, 512), (1, 512), (1, 512), (1, 512), (2, 1024), (1, 1024)]


def varint(n):
    out = bytearray()
    while n > 0x7F:
        out.append(n & 0x7F | 0x80)
        n >>= 7
    out.append(n)
    return bytes(out)


def num(field, n):
    return varint(field << 3) + varint(n)


def sub(field, *parts):
    body = b"".join(p.encode() if isinstance(p, str) else p for p in parts)
    return varint(field << 3 | 2) + varint(len(body)) + body


def ints(field, *ns):
    return sub(field, *(varint(n) for n in ns))


class Weights:
    def __init__(self):
        self.state = 0x4D4E7631

    def take(self, count, fan_in):
        """count fp16 values within a uniform range that keeps every layer's
        output near its input's size."""
        scale, s, out = (6 / fan_in) ** 0.5 / 2**31, self.state, [0.0] * count
        for i in range(count):
            s = (s * 6364136223846793005 + 1442695040888963407) & 0xFFFFFFFFFFFFFFFF
            out[i] = ((s >> 32) - 2**31) * scale
        self.state = s
        return sub(2, struct.pack(f"<{count}e", *out))


def layer(name, source, target, kind, *params):
    return sub(1, sub(1, name), sub(2, source), sub(3, target), sub(kind, *params))


def conv(w, name, source, target, c_in, c_out, kernel, stride, groups=1):
    per = c_in // groups
    return layer(name, source, target, 100, num(1, c_out), num(2, per), num(10, groups), ints(20, kernel, kernel),
                 ints(30, stride, stride), ints(40, 1, 1), sub(51), num(70, 1),
                 sub(90, w.take(c_out * per * kernel * kernel, per * kernel * kernel)), sub(91, w.take(c_out, 10000)))


def relu(name, source, target):
    return layer(name, source, target, 130, sub(10))


def feature(field, name, *shape):
    return sub(field, sub(1, name), sub(3, sub(5, ints(1, *shape), num(2, FLOAT32))))


def model():
    w, layers, channels = Weights(), [], 32
    layers += [conv(w, "conv0", "image", "conv0", 3, 32, 3, 2), relu("relu0", "conv0", "x0")]
    for i, (stride, out) in enumerate(BLOCKS, 1):
        layers += [
            conv(w, f"depthwise{i}", f"x{i - 1}", f"d{i}", channels, channels, 3, stride, channels),
            relu(f"depthwise{i}_relu", f"d{i}", f"dr{i}"),
            conv(w, f"pointwise{i}", f"dr{i}", f"p{i}", channels, out, 1, 1),
            relu(f"pointwise{i}_relu", f"p{i}", f"x{i}"),
        ]
        channels = out
    layers += [
        layer("pool", f"x{len(BLOCKS)}", "pooled", 120, num(1, 1), sub(30), num(60, 1)),
        conv(w, "classes", "pooled", "logits", channels, CLASSES, 1, 1),
        layer("softmax", "logits", "probabilities", 175),
    ]
    return num(1, SPEC_VERSION) + sub(2, feature(1, "image", 3, 224, 224), feature(10, "probabilities", CLASSES, 1, 1)) + sub(500, *layers)


if __name__ == "__main__":
    with open(sys.argv[1], "wb") as f:
        f.write(model())
