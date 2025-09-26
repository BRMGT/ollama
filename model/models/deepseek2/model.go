package deepseek2

// uses deepseek 2 architecture but written based on deepseek 3 model

import (
	"cmp"
	"fmt"
	"math"

	"github.com/ollama/ollama/fs"
	"github.com/ollama/ollama/kvcache"
	"github.com/ollama/ollama/ml"
	"github.com/ollama/ollama/ml/nn"
	"github.com/ollama/ollama/ml/nn/fast"
	"github.com/ollama/ollama/ml/nn/rope"
	"github.com/ollama/ollama/model"
	"github.com/ollama/ollama/model/input"
)

type Options struct {
	numExpertsUsed      int
	numExperts          int
	normTopKProb        bool
	routedScalingFactor float32

	kvLoraRank,
	qkNopeHeadDim,
	qkRopeHeadDim,
	kqNopeHeadDim,
	qkHeadDim int
	qLoraRank int
	vHeadDim  int

	hiddenSize,
	numHeads,
	numKVHeads,
	// keyLength,
	// valueLength,
	originalContextLength int

	eps,
	ropeBase,
	ropeScale float32
	kqScale float64
}

func (o Options) RoPEOptions() []func(*rope.Options) {
	attnFactor := float32(1.0 / (1.0 + 0.1*math.Log(float64(o.ropeScale))))
	return []func(*rope.Options){
		rope.WithOriginalContextLength(o.originalContextLength),
		rope.WithExtrapolationFactor(1.),
		rope.WithAttentionFactor(attnFactor),
	}
}

type Attention struct {
	Q *nn.Linear `gguf:"attn_q"`

	QA     *nn.Linear  `gguf:"attn_q_a"`
	QANorm *nn.RMSNorm `gguf:"attn_q_a_norm"`
	QB     *nn.Linear  `gguf:"attn_q_b"`

	KVA     *nn.Linear  `gguf:"attn_kv_a_mqa"`
	KVANorm *nn.RMSNorm `gguf:"attn_kv_a_norm"`
	KVB     *nn.Linear  `gguf:"attn_kv_b"`

	KB *nn.Linear `gguf:"attn_k_b"` // is this the best way to do it?
	VB *nn.Linear `gguf:"attn_v_b"` // i dont think this is the best way to do it?

	Output *nn.Linear `gguf:"attn_out,alt:attn_output"`
}

func (attn *Attention) Forward(ctx ml.Context, hiddenStates, positions ml.Tensor, cache kvcache.Cache, opts *Options) ml.Tensor {
	fmt.Printf("HELLO!\n")

	fmt.Printf("KVB layer: %v\n", attn.KVB)

	if attn.KVB != nil { // then we need to split it
		attn.KB = &nn.Linear{}
		attn.VB = &nn.Linear{}

		fmt.Printf("SPLITTING!\n")
		fmt.Printf("KVB shape: %v\n", attn.KVB.Weight.Shape())
		fmt.Printf("KVB stride 0: %v\n", attn.KVB.Weight.Stride(0))
		fmt.Printf("KVB stride 1: %v\n", attn.KVB.Weight.Stride(1))
		fmt.Printf("KVB stride 2: %v\n", attn.KVB.Weight.Stride(2))

		kvb := attn.KVB.Weight

		kvb = kvb.Reshape(ctx, 512, 256, 128)
		fmt.Printf("KVB shape: %v\n", kvb.Shape()) // [512, 256, 128]
		kvb = kvb.Permute(ctx, 1, 0, 2, 3)         //.Contiguous(ctx)
		fmt.Printf("KVB shape: %v\n", kvb.Shape()) // [256, 512, 128]

		kb := kvb.View(ctx,
			0, 128,
			kvb.Stride(1), kvb.Dim(1),
			kvb.Stride(2), kvb.Dim(2),
		)
		fmt.Printf("KB shape: %v\n", kb.Shape()) // [128, 512, 128]

		vb := kvb.View(ctx,
			128*kvb.Stride(0), 128,
			kvb.Stride(1), kvb.Dim(1),
			kvb.Stride(2), kvb.Dim(2),
		) // [128, 512, 128]

		vb = vb.Permute(ctx, 1, 0, 2, 3)         //.Contiguous(ctx)
		fmt.Printf("VB shape: %v\n", vb.Shape()) // [512, 128, 128]

		attn.KB.Weight = kb
		fmt.Printf("saved 1\n")
		attn.VB.Weight = vb
		fmt.Printf("saved 2\n")
	}

	fmt.Printf("Either completed or skipped splitting\n")

	seqLength := hiddenStates.Dim(1)

	var query ml.Tensor
	if opts.qLoraRank == 0 { // nil {
		query = attn.Q.Forward(ctx, hiddenStates)
	} else {
		query = attn.QA.Forward(ctx, hiddenStates)
		query = attn.QANorm.Forward(ctx, query, opts.eps)
		query = attn.QB.Forward(ctx, query)
	}

	fmt.Printf("query: %v\n", query.Shape())

	query = query.Reshape(ctx, query.Dim(0)/opts.numHeads, opts.numHeads, seqLength)

	qPass := query.View(ctx, 0,
		opts.qkNopeHeadDim, query.Stride(1),
		query.Dim(1), query.Stride(2),
		query.Dim(2))
	qRot := query.View(ctx, opts.qkNopeHeadDim*query.Stride(0),
		opts.qkRopeHeadDim, query.Stride(1),
		query.Dim(1), query.Stride(2),
		query.Dim(2))

	// fmt.Printf("QROT: %v\n", qRot.Shape())

	compressedKV := attn.KVA.Forward(ctx, hiddenStates)

	kPass := compressedKV.View(ctx, 0, opts.kvLoraRank, compressedKV.Stride(1), compressedKV.Dim(1))
	kRot := compressedKV.View(ctx, opts.kvLoraRank*compressedKV.Stride(0),
		opts.qkRopeHeadDim, compressedKV.Stride(1),
		1, compressedKV.Stride(1),
		compressedKV.Dim(1))

	qRot = fast.RoPE(ctx, qRot, positions, opts.qkRopeHeadDim, opts.ropeBase, 1./opts.ropeScale, opts.RoPEOptions()...)
	kRot = fast.RoPE(ctx, kRot, positions, opts.qkRopeHeadDim, opts.ropeBase, 1./opts.ropeScale, opts.RoPEOptions()...)

	kPass = attn.KVANorm.Forward(ctx, kPass, opts.eps)

	fmt.Printf("Finished all KVA operations\n")
	{
		// kPass = attn.KVB.Forward(ctx, kPass)

		// kv := kPass.Reshape(ctx, kPass.Dim(0)/opts.numKVHeads, opts.numKVHeads, seqLength)
		// kPass = kv.View(ctx, 0, opts.kqNopeHeadDim, kv.Stride(1), kv.Dim(1), kv.Stride(2), kv.Dim(2))
		// value := kv.View(ctx, opts.kqNopeHeadDim*kv.Stride(0),
		// 	opts.vHeadDim, kv.Stride(1),
		// 	kv.Dim(1), kv.Stride(2),
		// 	kv.Dim(2)).Contiguous(ctx)

		// // qRot = fast.RoPE(ctx, qRot, positions, opts.qkRopeHeadDim, opts.ropeBase, 1./opts.ropeScale, opts.RoPEOptions()...)
		// // kRot = fast.RoPE(ctx, kRot, positions, opts.qkRopeHeadDim, opts.ropeBase, 1./opts.ropeScale, opts.RoPEOptions()...)

		// kRot = kRot.Repeat(ctx, 1, qPass.Dim(1))

		// query = qRot.Concat(ctx, qPass, 0)
		// key := kRot.Concat(ctx, kPass, 0)

		// attention := nn.Attention(ctx, query, key, value, opts.kqScale, cache)
	}

	qPass = qPass.Permute(ctx, 0, 2, 1, 3)
	qPassAbsorb := attn.KB.Forward(ctx, qPass)
	qPassAbsorb = qPassAbsorb.Permute(ctx, 0, 2, 1, 3)

	query = qRot.Concat(ctx, qPassAbsorb, 0)
	fmt.Printf("query shape: %v\n", query.Shape())

	kPass = kPass.Reshape(ctx, opts.kvLoraRank, 1, seqLength)

	key := kRot.Concat(ctx, kPass, 0)
	fmt.Printf("key shape: %v\n", key.Shape())
	value := kPass
	fmt.Printf("value shape: %v\n", value.Shape())

	// attention := nn.Attention(ctx, query, key, value, opts.kqScale, cache)
	attention := nn.AttentionWithVMLA(ctx, query, key, value, nil, attn.VB.Weight, opts.kqScale, cache) // is there a better way to write this?
	fmt.Printf("attention shape: %v\n", attention.Shape())
	// func AttentionWithVMLA(ctx ml.Context, query, key, value, sinks ml.Tensor, vmla ml.Tensor, scale float64, cache kvcache.Cache) ml.Tensor {
	// the attention is where there is difference

	// attention := nn.Attention(ctx, query, key, value, opts.kqScale, cache)
	attention = attention.Reshape(ctx, attention.Dim(0)*attention.Dim(1), seqLength)
	fmt.Printf("attention shape: %v\n", attention.Shape())
	return attn.Output.Forward(ctx, attention)
}

type MLP interface {
	Forward(ml.Context, ml.Tensor, *Options) ml.Tensor
}

type sparse struct {
	Router       *nn.Linear `gguf:"ffn_gate_inp"`
	Gate         *nn.Linear `gguf:"ffn_gate_exps"`
	Up           *nn.Linear `gguf:"ffn_up_exps"`
	Down         *nn.Linear `gguf:"ffn_down_exps"`
	SharedExpert *dense     `gguf:",suf:_shexp"`
	ExpProbsBias ml.Tensor  `gguf:"exp_probs_b.bias,alt:exp_probs_b"`
}

func (moe *sparse) Moe(ctx ml.Context, hiddenStates, topKIndices, topKWeights ml.Tensor, opts *Options) ml.Tensor {
	hiddenStates = hiddenStates.Reshape(ctx, hiddenStates.Dim(0), 1, hiddenStates.Dim(1))

	upStates := moe.Up.Weight.MulmatID(ctx, hiddenStates, topKIndices)
	hiddenStates = moe.Gate.Weight.MulmatID(ctx, hiddenStates, topKIndices)
	hiddenStates = hiddenStates.SILU(ctx, upStates)

	experts := moe.Down.Weight.MulmatID(ctx, hiddenStates, topKIndices)
	experts = experts.Mul(ctx, topKWeights)
	nextStates := experts.View(ctx, 0, experts.Dim(0), experts.Stride(2), experts.Dim(2))
	for i := 1; i < opts.numExpertsUsed; i++ {
		nextStates = nextStates.Add(ctx, experts.View(ctx, i*experts.Stride(1), experts.Dim(0), experts.Stride(2), experts.Dim(2)))
	}
	return nextStates
}

func (moe *sparse) topKIndices(ctx ml.Context, scores ml.Tensor, opts *Options) ml.Tensor {
	scores = scores.Add(ctx, moe.ExpProbsBias)
	topKIndices := scores.TopK(ctx, opts.numExpertsUsed)
	return topKIndices
}

func (moe *sparse) Forward(ctx ml.Context, hiddenStates ml.Tensor, opts *Options) ml.Tensor {
	residuals := hiddenStates

	routerLogits := moe.Router.Forward(ctx, hiddenStates)
	scores := routerLogits.Sigmoid(ctx)
	topKIndices := moe.topKIndices(ctx, scores, opts)
	topKWeights := scores.Reshape(ctx, 1, opts.numExperts, hiddenStates.Dim(1)).Rows(ctx, topKIndices)

	if opts.normTopKProb {
		topKWeights = topKWeights.Reshape(ctx, opts.numExpertsUsed, hiddenStates.Dim(1))
		topKWeights = topKWeights.Div(ctx, topKWeights.SumRows(ctx))
		topKWeights = topKWeights.Reshape(ctx, 1, opts.numExpertsUsed, hiddenStates.Dim(1))
	}

	topKWeights = topKWeights.Scale(ctx, float64(opts.routedScalingFactor))
	hiddenStates = moe.Moe(ctx, hiddenStates, topKIndices, topKWeights, opts)
	sharedExpertResult := moe.SharedExpert.Forward(ctx, residuals, opts)

	hiddenStates = hiddenStates.Add(ctx, sharedExpertResult)
	return hiddenStates
}

type dense struct {
	Gate *nn.Linear `gguf:"ffn_gate"`
	Up   *nn.Linear `gguf:"ffn_up"`
	Down *nn.Linear `gguf:"ffn_down"`
}

func (mlp *dense) Forward(ctx ml.Context, hiddenStates ml.Tensor, opts *Options) ml.Tensor {
	hiddenStates = mlp.Gate.Forward(ctx, hiddenStates).SILU(ctx, mlp.Up.Forward(ctx, hiddenStates))
	return mlp.Down.Forward(ctx, hiddenStates)
}

type Layer struct {
	AttentionNorm *nn.RMSNorm `gguf:"attn_norm"`
	Attention     *Attention

	MLPNorm *nn.RMSNorm `gguf:"ffn_norm"`
	MLP     MLP
}

func (t *Layer) Forward(ctx ml.Context, hiddenStates, positions, outputs ml.Tensor, cache kvcache.Cache, opts *Options) ml.Tensor {
	residual := hiddenStates
	hiddenStates = t.AttentionNorm.Forward(ctx, hiddenStates, opts.eps)
	hiddenStates = t.Attention.Forward(ctx, hiddenStates, positions, cache, opts)

	if outputs != nil {
		hiddenStates = hiddenStates.Rows(ctx, outputs)
		residual = residual.Rows(ctx, outputs)
	}

	hiddenStates = hiddenStates.Add(ctx, residual)
	residual = hiddenStates

	hiddenStates = t.MLPNorm.Forward(ctx, hiddenStates, opts.eps)
	hiddenStates = t.MLP.Forward(ctx, hiddenStates, opts)
	hiddenStates = hiddenStates.Add(ctx, residual)
	return hiddenStates
}

type Model struct {
	model.Base
	model.BytePairEncoding

	TokenEmbedding *nn.Embedding `gguf:"token_embd"`
	Layers         []Layer       `gguf:"blk"`

	OutputNorm *nn.RMSNorm `gguf:"output_norm"`
	Output     *nn.Linear  `gguf:"output,alt:token_embd"`

	*Options
}

func New(c fs.Config) (model.Model, error) {
	layers := make([]Layer, c.Uint("block_count"))
	// layers := make([]Layer, 1)

	firstDenseLayerIndex := int(c.Uint("leading_dense_block_count"))
	for i := range layers {
		if i < firstDenseLayerIndex {
			layers[i].MLP = &dense{}
		} else {
			layers[i].MLP = &sparse{}
		}
	}

	mScale := float32(1.0 + float64(c.Float("rope.scaling.yarn_log_multiplier"))*math.Log(float64(c.Float("rope.scaling.factor"))))
	kqScale := float64(mScale) * float64(mScale) / math.Sqrt(float64(c.Uint("attention.key_length")))

	keyLength := cmp.Or(int(c.Uint("attention.key_length_mla")), int(c.Uint("attention.key_length")))
	valueLength := cmp.Or(int(c.Uint("attention.value_length_mla")), int(c.Uint("attention.value_length")))

	m := Model{
		BytePairEncoding: model.NewBytePairEncoding(
			&model.Vocabulary{
				Values: c.Strings("tokenizer.ggml.tokens"),
				Types:  c.Ints("tokenizer.ggml.token_type"),
				Merges: c.Strings("tokenizer.ggml.merges"),
				AddBOS: c.Bool("tokenizer.ggml.add_bos_token", true),
				BOS:    []int32{int32(c.Uint("tokenizer.ggml.bos_token_id"))},
				AddEOS: c.Bool("tokenizer.ggml.add_eos_token", false),
				EOS: append(
					[]int32{int32(c.Uint("tokenizer.ggml.eos_token_id"))},
					c.Ints("tokenizer.ggml.eos_token_ids")...,
				),
			},
			// Split regex into multiple parts (according to DeepSeek3's regex)
			"\\p{N}{1,3}",
			`[一-龥぀-ゟ゠-ヿ]+`,
			"[!\"#$%&'()*+,\\-./:;<=>?@\\[\\\\\\]^_`{|}~][A-Za-z]+|[^\r\n\\p{L}\\p{P}\\p{S}]?[\\p{L}\\p{M}]+| ?[\\p{P}\\p{S}]+[\r\n]*|\\s*[\r\n]+|\\s+(?!\\S)|\\s+",
		),
		Layers: layers,
		Options: &Options{
			hiddenSize: int(c.Uint("embedding_length")),
			numHeads:   int(c.Uint("attention.head_count")),
			numKVHeads: int(c.Uint("attention.head_count_kv")),
			// keyLength:      keyLength,
			// valueLength:    valueLength,
			eps:            c.Float("attention.layer_norm_rms_epsilon"),
			ropeBase:       c.Float("rope.freq_base"),
			ropeScale:      c.Float("rope.scaling.factor", 1),
			numExperts:     int(c.Uint("expert_count")),
			numExpertsUsed: int(c.Uint("expert_used_count")),
			normTopKProb:   c.Bool("expert_weights_norm", true),

			qLoraRank:     int(c.Uint("attention.q_lora_rank")), //&qLoraRankVal,
			kvLoraRank:    int(c.Uint("attention.kv_lora_rank")),
			qkHeadDim:     keyLength,
			vHeadDim:      valueLength,
			qkRopeHeadDim: int(c.Uint("rope.dimension_count")),
			qkNopeHeadDim: keyLength - int(c.Uint("rope.dimension_count")),
			kqNopeHeadDim: keyLength - int(c.Uint("rope.dimension_count")),

			routedScalingFactor:   c.Float("expert_weights_scale"),
			originalContextLength: int(c.Uint("rope.scaling.original_context_length")),

			kqScale: kqScale,
		},
	}

	m.Cache = kvcache.NewCausalCache(m.Shift)
	return &m, nil
}

func (m Model) Shift(ctx ml.Context, layer int, key, shift ml.Tensor) (ml.Tensor, error) {
	return fast.RoPE(ctx, key, shift, m.qkRopeHeadDim, m.ropeBase, 1./m.ropeScale, m.RoPEOptions()...), nil
}

func (m *Model) Forward(ctx ml.Context, batch input.Batch) (ml.Tensor, error) {
	positions := ctx.Input().FromIntSlice(batch.Positions, len(batch.Positions))

	hiddenStates := m.TokenEmbedding.Forward(ctx, batch.Inputs)

	for i, layer := range m.Layers {
		m.Cache.SetLayer(i)

		var outputs ml.Tensor
		if i == len(m.Layers)-1 {
			outputs = batch.Outputs
		}

		hiddenStates = layer.Forward(ctx, hiddenStates, positions, outputs, m.Cache, m.Options)
	}

	hiddenStates = m.OutputNorm.Forward(ctx, hiddenStates, m.eps)
	return m.Output.Forward(ctx, hiddenStates), nil
}

func init() {
	model.Register("deepseek2", New)
}
