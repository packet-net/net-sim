// Reference vectors for net-sim's Go port of M0LTE.FmChannel (internal/fm).
//
// Two kinds, because the two implementations share a design but not a random
// number generator:
//   - noiseless: the same audio through the same profile must come out the
//     same, sample for sample, so every filter, the emphasis pair, the
//     modulator and the discriminator are pinned exactly;
//   - statistical: SINAD of a 1 kHz tone, and the output noise of an
//     unmodulated carrier, against carrier-to-noise ratio, averaged over seeds,
//     which pins the noise, the IF filter's noise bandwidth and the threshold.
//
// Every profile is driven at fixed gain (LimitAtDeviationHz set far above any
// peak): a live link can't scale each burst to its own peak, so the Go port
// only has fixed gain, and that is the mode compared.
using System.Text.Json;
using M0LTE.Fm;

string outDir = args.Length > 0 ? args[0] : Path.Combine("..", "..", "internal", "fm", "testdata");
Directory.CreateDirectory(outDir);
const int Rate = 48000;
const double NoLimit = 1e6;

var profiles = new Dictionary<string, FmLinkProfile>
{
    ["data8k"] = FmLinkProfile.DataPort(2500) with { LimitAtDeviationHz = NoLimit },
    ["data16k"] = FmLinkProfile.DataPort(5000, ifBandwidthHz: 16000) with { LimitAtDeviationHz = NoLimit },
    ["mic8k"] = FmLinkProfile.MicAndSpeaker(2500) with { LimitAtDeviationHz = NoLimit },
};

var manifest = new Dictionary<string, object>();
var profileOut = new Dictionary<string, object>();
foreach (var (name, p) in profiles)
{
    profileOut[name] = new
    {
        p.PeakDeviationHz, p.IfBandwidthHz, p.TxAudioLowHz, p.TxAudioHighHz,
        p.RxAudioLowHz, p.RxAudioHighHz, p.PreEmphasisMicroseconds, p.DeEmphasisMicroseconds,
        InterpolationFactor = new FmChannel(p, Rate, 1).InterpolationFactor,
    };
}
manifest["rate"] = Rate;
manifest["package"] = "M0LTE.FmChannel 0.7.0";
manifest["profiles"] = profileOut;

// Noiseless: a multitone inside every profile's passband.
int n = Rate / 2;
var input = new float[n];
for (int i = 0; i < n; i++)
{
    double t = (double)i / Rate;
    input[i] = (float)(0.25 * Math.Sin(2 * Math.PI * 700 * t)
        + 0.2 * Math.Sin(2 * Math.PI * 1900 * t)
        + 0.1 * Math.Sin(2 * Math.PI * 2700 * t));
}
WriteF32(Path.Combine(outDir, "noiseless-in.f32"), input);
foreach (var (name, p) in profiles)
{
    float[] heard = new FmChannel(p, Rate, 1).Apply(input, double.PositiveInfinity, 0, 0);
    WriteF32(Path.Combine(outDir, $"noiseless-{name}.f32"), heard);
}

// Statistical.
double[] cnrs = Enumerable.Range(0, 17).Select(k => -2.0 + 2 * k).ToArray();
const int Seeds = 4;
var sinad = new Dictionary<string, object>();
var quiet = new Dictionary<string, object>();
foreach (var (name, p) in profiles)
{
    int len = (int)(1.5 * Rate);
    var tone = new float[len];
    for (int i = 0; i < len; i++)
    {
        tone[i] = (float)(0.6 * Math.Sin(2 * Math.PI * 1000 * i / Rate));
    }
    var silence = new float[len];
    var sRows = new List<object>();
    var qRows = new List<object>();
    foreach (double cnr in cnrs)
    {
        double s = 0, q = 0;
        for (int seed = 1; seed <= Seeds; seed++)
        {
            s += Sinad(new FmChannel(p, Rate, seed).Apply(tone, cnr, 0, 0), Rate);
            q += MeanSquare(new FmChannel(p, Rate, seed).Apply(silence, cnr, 0, 0), Rate);
        }
        sRows.Add(new { cnr, sinad_db = s / Seeds });
        qRows.Add(new { cnr, noise_db = 10 * Math.Log10(q / Seeds) });
    }
    sinad[name] = sRows;
    quiet[name] = qRows;
}
manifest["sinad_1khz_60pct"] = sinad;
manifest["unmodulated_noise"] = quiet;
File.WriteAllText(Path.Combine(outDir, "reference.json"),
    JsonSerializer.Serialize(manifest, new JsonSerializerOptions { WriteIndented = true }));
Console.WriteLine($"wrote reference vectors to {outDir}");

// Measurement window: skip filter start-up, and the same window in Go.
static (int Start, int End) Window(int length, int rate) => (rate * 3 / 10, length - rate / 20);

// SINAD of a 1 kHz tone: total power over the power left after removing the
// least-squares 1 kHz sinusoid. Mirrored exactly in internal/fm's tests.
static double Sinad(float[] x, int rate)
{
    (int a, int b) = Window(x.Length, rate);
    double ss = 0, sc = 0, cc = 0, xs = 0, xc = 0, total = 0, mean = 0;
    for (int i = a; i < b; i++) mean += x[i];
    mean /= b - a;
    for (int i = a; i < b; i++)
    {
        double s = Math.Sin(2 * Math.PI * 1000 * i / rate), c = Math.Cos(2 * Math.PI * 1000 * i / rate);
        double v = x[i] - mean;
        ss += s * s; cc += c * c; sc += s * c; xs += v * s; xc += v * c; total += v * v;
    }
    double det = (ss * cc) - (sc * sc);
    double A = ((xs * cc) - (xc * sc)) / det, B = ((xc * ss) - (xs * sc)) / det;
    double residual = 0;
    for (int i = a; i < b; i++)
    {
        double s = Math.Sin(2 * Math.PI * 1000 * i / rate), c = Math.Cos(2 * Math.PI * 1000 * i / rate);
        double r = x[i] - mean - (A * s) - (B * c);
        residual += r * r;
    }
    return 10 * Math.Log10(total / residual);
}

static double MeanSquare(float[] x, int rate)
{
    (int a, int b) = Window(x.Length, rate);
    double sum = 0;
    for (int i = a; i < b; i++) sum += (double)x[i] * x[i];
    return sum / (b - a);
}

static void WriteF32(string path, float[] data)
{
    using var w = new BinaryWriter(File.Create(path));
    foreach (float f in data) w.Write(f);
}
