// ane-bench: a compiled Core ML model's inferences per second with compute
// units CPU and Neural Engine, and where Core ML placed it.
//
//   ane-bench MODEL.mlmodelc [--warmup N] [--iterations N]
//
// Inputs are zeros of the model's own shapes, batch as the model has it.
// Placement comes from MLComputePlan (macOS 14.4 and later): each operation's
// preferred device, counted; "neural_engine" only when every placed
// operation prefers it. Older macOS reports "unknown" with a reason.
// model_sha256 is the sha256 of the lines `shasum -a 256` gives for every
// file of the model, as `find . -type f | LC_ALL=C sort` lists them inside
// it. Prints one JSON line; on failure {"error":CODE} and exit status 1
// (2 for usage).
import CoreML
import CoreVideo
import CryptoKit
import Foundation

struct Failure: Error {
    let code: String
}

func emit(_ fields: [String: Any]) {
    let data = try! JSONSerialization.data(withJSONObject: fields, options: [.sortedKeys])
    print(String(decoding: data, as: UTF8.self))
}

func hex<D: Digest>(_ d: D) -> String {
    d.map { String(format: "%02x", $0) }.joined()
}

func modelDigest(_ root: URL) throws -> String {
    guard let walk = FileManager.default.enumerator(at: root, includingPropertiesForKeys: [.isRegularFileKey]) else {
        throw Failure(code: "digest")
    }
    let base = root.standardizedFileURL.path
    var files: [String] = []
    for case let url as URL in walk {
        guard (try? url.resourceValues(forKeys: [.isRegularFileKey]))?.isRegularFile == true else { continue }
        let path = url.standardizedFileURL.path
        guard path.hasPrefix(base + "/") else { throw Failure(code: "digest") }
        files.append(String(path.dropFirst(base.count + 1)))
    }
    files.sort { Array($0.utf8).lexicographicallyPrecedes(Array($1.utf8)) }
    var lines = ""
    for rel in files {
        guard let data = FileManager.default.contents(atPath: base + "/" + rel) else { throw Failure(code: "digest") }
        lines += "\(hex(SHA256.hash(data: data)))  ./\(rel)\n"
    }
    return hex(SHA256.hash(data: Data(lines.utf8)))
}

func zeros(_ description: MLModelDescription) throws -> MLFeatureProvider {
    var features: [String: MLFeatureValue] = [:]
    for (name, input) in description.inputDescriptionsByName {
        switch input.type {
        case .multiArray:
            guard let c = input.multiArrayConstraint else { throw Failure(code: "input_type") }
            let array = try MLMultiArray(shape: c.shape, dataType: c.dataType)
            for i in 0..<array.count { array[i] = 0 }
            features[name] = MLFeatureValue(multiArray: array)
        case .image:
            guard let c = input.imageConstraint else { throw Failure(code: "input_type") }
            var buffer: CVPixelBuffer?
            guard CVPixelBufferCreate(nil, c.pixelsWide, c.pixelsHigh, c.pixelFormatType, nil, &buffer) == kCVReturnSuccess,
                  let pixels = buffer else { throw Failure(code: "input_type") }
            CVPixelBufferLockBaseAddress(pixels, [])
            if let p = CVPixelBufferGetBaseAddress(pixels) {
                memset(p, 0, CVPixelBufferGetDataSize(pixels))
            }
            CVPixelBufferUnlockBaseAddress(pixels, [])
            features[name] = MLFeatureValue(pixelBuffer: pixels)
        default:
            throw Failure(code: "input_type")
        }
    }
    return try MLDictionaryFeatureProvider(dictionary: features)
}

struct Placement {
    var ops = 0
    var neuralEngine = 0
}

@available(macOS 14.4, *)
func count(_ usage: MLComputePlan.DeviceUsage?, into p: inout Placement) {
    guard let usage else { return }
    p.ops += 1
    if case .neuralEngine = usage.preferred { p.neuralEngine += 1 }
}

@available(macOS 14.4, *)
func walk(_ block: MLModelStructure.Program.Block, _ plan: MLComputePlan, into p: inout Placement) {
    for op in block.operations {
        count(plan.deviceUsage(for: op), into: &p)
        for inner in op.blocks { walk(inner, plan, into: &p) }
    }
}

@available(macOS 14.4, *)
func walk(_ structure: MLModelStructure, _ plan: MLComputePlan, into p: inout Placement) {
    switch structure {
    case .neuralNetwork(let nn):
        for layer in nn.layers { count(plan.deviceUsage(for: layer), into: &p) }
    case .program(let program):
        for function in program.functions.values { walk(function.block, plan, into: &p) }
    case .pipeline(let pipeline):
        for model in pipeline.subModels { walk(model, plan, into: &p) }
    default:
        break
    }
}

func placement(_ url: URL, _ config: MLModelConfiguration) async -> [String: Any] {
    guard #available(macOS 14.4, *) else {
        return ["placement": "unknown", "placement_reason": "no_compute_plan"]
    }
    guard let plan = try? await MLComputePlan.load(contentsOf: url, configuration: config) else {
        return ["placement": "unknown", "placement_reason": "compute_plan_failed"]
    }
    var p = Placement()
    walk(plan.modelStructure, plan, into: &p)
    let where_: String
    if p.ops == 0 {
        return ["placement": "unknown", "placement_reason": "no_placed_ops"]
    } else if p.neuralEngine == p.ops {
        where_ = "neural_engine"
    } else if p.neuralEngine > 0 {
        where_ = "partial"
    } else {
        where_ = "not_neural_engine"
    }
    return ["placement": where_, "ops": p.ops, "ops_neural_engine": p.neuralEngine]
}

func number(_ s: String, _ least: Int) throws -> Int {
    guard let n = Int(s), n >= least, n <= 1_000_000 else { throw Failure(code: "usage") }
    return n
}

func run(_ args: [String]) async throws -> [String: Any] {
    var model: String?
    var warmup = 50
    var iterations = 1000
    var i = 0
    while i < args.count {
        switch args[i] {
        case "--warmup" where i + 1 < args.count:
            warmup = try number(args[i + 1], 0)
            i += 2
        case "--iterations" where i + 1 < args.count:
            iterations = try number(args[i + 1], 1)
            i += 2
        case let a where model == nil && !a.hasPrefix("-"):
            model = a
            i += 1
        default:
            throw Failure(code: "usage")
        }
    }
    guard let model else { throw Failure(code: "usage") }
    let url = URL(fileURLWithPath: model)

    let config = MLModelConfiguration()
    config.computeUnits = .cpuAndNeuralEngine
    let sha = try modelDigest(url)
    guard let loaded = try? MLModel(contentsOf: url, configuration: config) else { throw Failure(code: "model_load") }
    let input = try zeros(loaded.modelDescription)
    let options = MLPredictionOptions()

    do {
        for _ in 0..<warmup { _ = try loaded.prediction(from: input, options: options) }
        let start = DispatchTime.now().uptimeNanoseconds
        for _ in 0..<iterations { _ = try loaded.prediction(from: input, options: options) }
        let seconds = Double(DispatchTime.now().uptimeNanoseconds - start) / 1e9
        var out: [String: Any] = [
            "model_sha256": sha,
            "compute_units": "cpu_and_neural_engine",
            "warmup": warmup,
            "iterations": iterations,
            "seconds": seconds,
            "inferences_per_s": Double(iterations) / seconds,
        ]
        out.merge(await placement(url, config)) { a, _ in a }
        return out
    } catch {
        throw Failure(code: "predict")
    }
}

@main
struct ANEBench {
    static func main() async {
        do {
            emit(try await run(Array(CommandLine.arguments.dropFirst())))
        } catch let f as Failure {
            emit(["error": f.code])
            if f.code == "usage" {
                FileHandle.standardError.write(Data("usage: ane-bench MODEL.mlmodelc [--warmup N] [--iterations N]\n".utf8))
            }
            exit(f.code == "usage" ? 2 : 1)
        } catch {
            emit(["error": "model_load"])
            exit(1)
        }
    }
}
