package script

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/dop251/goja"
)

const maxTrainingSamples = 10000

// registerML injects the `ml` global object into the Goja VM with lightweight
// machine learning models that can be trained and used for prediction in JS.
func registerML(vm *goja.Runtime) {
	ml := vm.NewObject()

	// -----------------------------------------------------------------------
	// Linear Regression (multi-variate)
	// -----------------------------------------------------------------------

	// ml.linearRegression(features, targets) → model
	ml.Set("linearRegression", func(call goja.FunctionCall) goja.Value {
		features := toFloat64Matrix(call.Argument(0).Export())
		targets := toFloat64Slice(call.Argument(1).Export())
		n := minLen(len(features), len(targets))
		if n < 2 {
			panic(vm.NewGoError(errInsufficientData))
		}
		if n > maxTrainingSamples {
			n = maxTrainingSamples
		}
		features = features[:n]
		targets = targets[:n]

		coeffs, intercept := fitLinearRegression(features, targets)

		model := vm.NewObject()
		model.Set("coefficients", vm.ToValue(coeffs))
		model.Set("intercept", intercept)
		model.Set("predict", func(call goja.FunctionCall) goja.Value {
			x := toFloat64Slice(call.Argument(0).Export())
			pred := intercept
			for i := 0; i < len(coeffs) && i < len(x); i++ {
				pred += coeffs[i] * x[i]
			}
			return vm.ToValue(pred)
		})
		return model
	})

	// -----------------------------------------------------------------------
	// Logistic Regression (binary classification)
	// -----------------------------------------------------------------------

	// ml.logisticRegression(features, labels, opts?) → model
	ml.Set("logisticRegression", func(call goja.FunctionCall) goja.Value {
		features := toFloat64Matrix(call.Argument(0).Export())
		labels := toFloat64Slice(call.Argument(1).Export())
		opts := exportOpts(call.Argument(2).Export())
		maxIter := intOpt(opts, "maxIter", 100)
		lr := floatOpt(opts, "lr", 0.01)

		n := minLen(len(features), len(labels))
		if n < 2 {
			panic(vm.NewGoError(errInsufficientData))
		}
		if n > maxTrainingSamples {
			n = maxTrainingSamples
		}
		features = features[:n]
		labels = labels[:n]

		dim := len(features[0])
		weights := make([]float64, dim)
		bias := 0.0

		for iter := 0; iter < maxIter; iter++ {
			dw := make([]float64, dim)
			db := 0.0
			for i := 0; i < n; i++ {
				z := bias
				for j := 0; j < dim; j++ {
					z += weights[j] * features[i][j]
				}
				pred := sigmoid(z)
				err := pred - labels[i]
				for j := 0; j < dim; j++ {
					dw[j] += err * features[i][j]
				}
				db += err
			}
			for j := 0; j < dim; j++ {
				weights[j] -= lr * dw[j] / float64(n)
			}
			bias -= lr * db / float64(n)
		}

		model := vm.NewObject()
		model.Set("predict", func(call goja.FunctionCall) goja.Value {
			x := toFloat64Slice(call.Argument(0).Export())
			z := bias
			for j := 0; j < len(weights) && j < len(x); j++ {
				z += weights[j] * x[j]
			}
			return vm.ToValue(sigmoid(z))
		})
		return model
	})

	// -----------------------------------------------------------------------
	// Decision Tree (classification)
	// -----------------------------------------------------------------------

	// ml.decisionTree(features, labels, opts?) → model
	ml.Set("decisionTree", func(call goja.FunctionCall) goja.Value {
		features := toFloat64Matrix(call.Argument(0).Export())
		labels := toFloat64Slice(call.Argument(1).Export())
		opts := exportOpts(call.Argument(2).Export())
		maxDepth := intOpt(opts, "maxDepth", 5)
		minSamples := intOpt(opts, "minSamples", 10)

		n := minLen(len(features), len(labels))
		if n < 2 {
			panic(vm.NewGoError(errInsufficientData))
		}
		if n > maxTrainingSamples {
			n = maxTrainingSamples
		}

		indices := makeRange(n)
		tree := buildTree(features[:n], labels[:n], indices, maxDepth, minSamples, 0)

		model := vm.NewObject()
		model.Set("predict", func(call goja.FunctionCall) goja.Value {
			x := toFloat64Slice(call.Argument(0).Export())
			return vm.ToValue(predictTree(tree, x))
		})
		return model
	})

	// -----------------------------------------------------------------------
	// Random Forest (classification)
	// -----------------------------------------------------------------------

	// ml.randomForest(features, labels, opts?) → model
	// opts.seed (default 42): random seed for bootstrap sampling. Use different
	// seeds to explore randomness across backtests. Set to 0 for time-based seed.
	ml.Set("randomForest", func(call goja.FunctionCall) goja.Value {
		features := toFloat64Matrix(call.Argument(0).Export())
		labels := toFloat64Slice(call.Argument(1).Export())
		opts := exportOpts(call.Argument(2).Export())
		nTrees := intOpt(opts, "nTrees", 50)
		maxDepth := intOpt(opts, "maxDepth", 5)
		minSamples := intOpt(opts, "minSamples", 10)
		seed := int64(intOpt(opts, "seed", 42))

		n := minLen(len(features), len(labels))
		if n < 2 {
			panic(vm.NewGoError(errInsufficientData))
		}
		if n > maxTrainingSamples {
			n = maxTrainingSamples
		}
		features = features[:n]
		labels = labels[:n]

		if seed == 0 {
			seed = time.Now().UnixNano()
		}
		rng := rand.New(rand.NewSource(seed))
		trees := make([]*treeNode, nTrees)
		for t := 0; t < nTrees; t++ {
			// Bootstrap sample
			indices := make([]int, n)
			for i := range indices {
				indices[i] = rng.Intn(n)
			}
			trees[t] = buildTree(features, labels, indices, maxDepth, minSamples, 0)
		}

		model := vm.NewObject()
		model.Set("predict", func(call goja.FunctionCall) goja.Value {
			x := toFloat64Slice(call.Argument(0).Export())
			votes := 0.0
			for _, tree := range trees {
				votes += predictTree(tree, x)
			}
			avg := votes / float64(len(trees))
			if avg >= 0.5 {
				return vm.ToValue(1.0)
			}
			return vm.ToValue(0.0)
		})
		model.Set("predictProba", func(call goja.FunctionCall) goja.Value {
			x := toFloat64Slice(call.Argument(0).Export())
			votes := 0.0
			for _, tree := range trees {
				votes += predictTree(tree, x)
			}
			return vm.ToValue(votes / float64(len(trees)))
		})
		return model
	})

	// -----------------------------------------------------------------------
	// K-Nearest Neighbors (classification)
	// -----------------------------------------------------------------------

	// ml.knn(features, labels, opts?) → model
	ml.Set("knn", func(call goja.FunctionCall) goja.Value {
		features := toFloat64Matrix(call.Argument(0).Export())
		labels := toFloat64Slice(call.Argument(1).Export())
		opts := exportOpts(call.Argument(2).Export())
		k := intOpt(opts, "k", 5)

		n := minLen(len(features), len(labels))
		if n < 1 {
			panic(vm.NewGoError(errInsufficientData))
		}
		if n > maxTrainingSamples {
			n = maxTrainingSamples
		}
		features = features[:n]
		labels = labels[:n]
		if k > n {
			k = n
		}

		model := vm.NewObject()
		model.Set("predict", func(call goja.FunctionCall) goja.Value {
			x := toFloat64Slice(call.Argument(0).Export())
			// Compute distances to all training points
			type distLabel struct {
				dist  float64
				label float64
			}
			dists := make([]distLabel, n)
			for i := 0; i < n; i++ {
				d := euclideanDist(x, features[i])
				dists[i] = distLabel{d, labels[i]}
			}
			sort.Slice(dists, func(a, b int) bool {
				return dists[a].dist < dists[b].dist
			})
			votes := 0.0
			for i := 0; i < k; i++ {
				votes += dists[i].label
			}
			if votes >= float64(k)/2.0 {
				return vm.ToValue(1.0)
			}
			return vm.ToValue(0.0)
		})
		return model
	})

	// -----------------------------------------------------------------------
	// K-Means Clustering
	// -----------------------------------------------------------------------

	// ml.kmeans(features, opts?) → clusters
	ml.Set("kmeans", func(call goja.FunctionCall) goja.Value {
		features := toFloat64Matrix(call.Argument(0).Export())
		opts := exportOpts(call.Argument(1).Export())
		k := intOpt(opts, "k", 3)
		maxIter := intOpt(opts, "maxIter", 100)

		n := len(features)
		if n < k || n < 1 {
			panic(vm.NewGoError(errInsufficientData))
		}
		if n > maxTrainingSamples {
			n = maxTrainingSamples
			features = features[:n]
		}
		dim := len(features[0])

		// Initialize centers with first k distinct points (deterministic by default).
		seed := int64(intOpt(opts, "seed", 42))
		if seed == 0 {
			seed = time.Now().UnixNano()
		}
		rng := rand.New(rand.NewSource(seed))
		perm := rng.Perm(n)
		centers := make([][]float64, k)
		for i := 0; i < k; i++ {
			centers[i] = make([]float64, dim)
			copy(centers[i], features[perm[i]])
		}

		assignments := make([]int, n)

		for iter := 0; iter < maxIter; iter++ {
			changed := false
			// Assign
			for i := 0; i < n; i++ {
				best := 0
				bestDist := euclideanDist(features[i], centers[0])
				for c := 1; c < k; c++ {
					d := euclideanDist(features[i], centers[c])
					if d < bestDist {
						bestDist = d
						best = c
					}
				}
				if assignments[i] != best {
					changed = true
					assignments[i] = best
				}
			}
			if !changed {
				break
			}
			// Update centers
			counts := make([]int, k)
			newCenters := make([][]float64, k)
			for c := 0; c < k; c++ {
				newCenters[c] = make([]float64, dim)
			}
			for i := 0; i < n; i++ {
				c := assignments[i]
				counts[c]++
				for j := 0; j < dim; j++ {
					newCenters[c][j] += features[i][j]
				}
			}
			for c := 0; c < k; c++ {
				if counts[c] > 0 {
					for j := 0; j < dim; j++ {
						newCenters[c][j] /= float64(counts[c])
					}
					centers[c] = newCenters[c]
				}
			}
		}

		// Export centers as [][]float64
		centersJS := make([]interface{}, k)
		for i, c := range centers {
			centersJS[i] = c
		}

		model := vm.NewObject()
		model.Set("centers", vm.ToValue(centersJS))
		model.Set("predict", func(call goja.FunctionCall) goja.Value {
			x := toFloat64Slice(call.Argument(0).Export())
			best := 0
			bestDist := euclideanDist(x, centers[0])
			for c := 1; c < k; c++ {
				d := euclideanDist(x, centers[c])
				if d < bestDist {
					bestDist = d
					best = c
				}
			}
			return vm.ToValue(best)
		})
		return model
	})

	// -----------------------------------------------------------------------
	// Utilities
	// -----------------------------------------------------------------------

	// ml.normalize(features) → number[][] (min-max scaling to [0,1])
	ml.Set("normalize", func(call goja.FunctionCall) goja.Value {
		features := toFloat64Matrix(call.Argument(0).Export())
		if len(features) == 0 {
			return vm.ToValue([]interface{}{})
		}
		return vm.ToValue(normalizeMatrix(features))
	})

	// ml.standardize(features) → number[][] (z-score scaling)
	ml.Set("standardize", func(call goja.FunctionCall) goja.Value {
		features := toFloat64Matrix(call.Argument(0).Export())
		if len(features) == 0 {
			return vm.ToValue([]interface{}{})
		}
		return vm.ToValue(standardizeMatrix(features))
	})

	// ml.trainTestSplit(features, labels, opts?) → { trainFeatures, trainLabels, testFeatures, testLabels }
	ml.Set("trainTestSplit", func(call goja.FunctionCall) goja.Value {
		features := toFloat64Matrix(call.Argument(0).Export())
		labels := toFloat64Slice(call.Argument(1).Export())
		opts := exportOpts(call.Argument(2).Export())
		testSize := floatOpt(opts, "testSize", 0.2)

		n := minLen(len(features), len(labels))
		if n < 2 {
			panic(vm.NewGoError(errInsufficientData))
		}

		splitIdx := int(float64(n) * (1 - testSize))
		if splitIdx < 1 {
			splitIdx = 1
		}
		if splitIdx >= n {
			splitIdx = n - 1
		}

		result := vm.NewObject()
		result.Set("trainFeatures", vm.ToValue(matrixToInterface(features[:splitIdx])))
		result.Set("trainLabels", vm.ToValue(labels[:splitIdx]))
		result.Set("testFeatures", vm.ToValue(matrixToInterface(features[splitIdx:n])))
		result.Set("testLabels", vm.ToValue(labels[splitIdx:n]))
		return result
	})

	vm.Set("ml", ml)
}

// -----------------------------------------------------------------------
// Internal ML helpers
// -----------------------------------------------------------------------

var errInsufficientData = fmt.Errorf("ml: insufficient training data")

// toFloat64Matrix converts a JS 2D array export to [][]float64.
func toFloat64Matrix(v interface{}) [][]float64 {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([][]float64, 0, len(arr))
	for _, row := range arr {
		out = append(out, toFloat64Slice(row))
	}
	return out
}

// matrixToInterface converts [][]float64 to []interface{} for Goja export.
func matrixToInterface(m [][]float64) []interface{} {
	out := make([]interface{}, len(m))
	for i, row := range m {
		out[i] = row
	}
	return out
}

// exportOpts safely converts a JS options object to map[string]interface{}.
func exportOpts(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

func intOpt(opts map[string]interface{}, key string, def int) int {
	if opts == nil {
		return def
	}
	v, ok := opts[key]
	if !ok {
		return def
	}
	return toInt(v)
}

func floatOpt(opts map[string]interface{}, key string, def float64) float64 {
	if opts == nil {
		return def
	}
	v, ok := opts[key]
	if !ok {
		return def
	}
	return toFloat(v)
}

func sigmoid(z float64) float64 {
	return 1.0 / (1.0 + math.Exp(-z))
}

func euclideanDist(a, b []float64) float64 {
	sum := 0.0
	n := minLen(len(a), len(b))
	for i := 0; i < n; i++ {
		d := a[i] - b[i]
		sum += d * d
	}
	return math.Sqrt(sum)
}

func makeRange(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// -----------------------------------------------------------------------
// Linear Regression (OLS via normal equations with pseudo-inverse)
// -----------------------------------------------------------------------

func fitLinearRegression(features [][]float64, targets []float64) ([]float64, float64) {
	n := len(features)
	if n == 0 {
		return nil, 0
	}
	dim := len(features[0])

	// Add bias column: X = [features | 1]
	// Solve via: beta = (X'X)^-1 X'y using gradient descent for simplicity
	// (avoids matrix inversion code)
	weights := make([]float64, dim)
	bias := 0.0
	lr := 0.001
	// Normalize features for stable convergence
	means := make([]float64, dim)
	stds := make([]float64, dim)
	for j := 0; j < dim; j++ {
		for i := 0; i < n; i++ {
			means[j] += features[i][j]
		}
		means[j] /= float64(n)
		for i := 0; i < n; i++ {
			d := features[i][j] - means[j]
			stds[j] += d * d
		}
		stds[j] = math.Sqrt(stds[j] / float64(n))
		if stds[j] == 0 {
			stds[j] = 1
		}
	}

	// Normalized features
	normF := make([][]float64, n)
	for i := 0; i < n; i++ {
		normF[i] = make([]float64, dim)
		for j := 0; j < dim; j++ {
			normF[i][j] = (features[i][j] - means[j]) / stds[j]
		}
	}

	for iter := 0; iter < 1000; iter++ {
		dw := make([]float64, dim)
		db := 0.0
		for i := 0; i < n; i++ {
			pred := bias
			for j := 0; j < dim; j++ {
				pred += weights[j] * normF[i][j]
			}
			err := pred - targets[i]
			for j := 0; j < dim; j++ {
				dw[j] += err * normF[i][j]
			}
			db += err
		}
		for j := 0; j < dim; j++ {
			weights[j] -= lr * dw[j] / float64(n)
		}
		bias -= lr * db / float64(n)
	}

	// Un-normalize weights
	realWeights := make([]float64, dim)
	realBias := bias
	for j := 0; j < dim; j++ {
		realWeights[j] = weights[j] / stds[j]
		realBias -= weights[j] * means[j] / stds[j]
	}

	return realWeights, realBias
}

// -----------------------------------------------------------------------
// Decision Tree
// -----------------------------------------------------------------------

type treeNode struct {
	isLeaf   bool
	value    float64 // leaf prediction (majority class)
	feature  int     // split feature index
	thresh   float64 // split threshold
	left     *treeNode
	right    *treeNode
}

func buildTree(features [][]float64, labels []float64, indices []int, maxDepth, minSamples, depth int) *treeNode {
	if len(indices) == 0 {
		return &treeNode{isLeaf: true, value: 0}
	}

	// Majority class
	ones := 0.0
	for _, idx := range indices {
		ones += labels[idx]
	}
	majority := 0.0
	if ones >= float64(len(indices))/2.0 {
		majority = 1.0
	}

	// Leaf conditions
	if depth >= maxDepth || len(indices) < minSamples || ones == 0 || ones == float64(len(indices)) {
		return &treeNode{isLeaf: true, value: majority}
	}

	dim := len(features[0])
	bestGini := math.Inf(1)
	bestFeature := 0
	bestThresh := 0.0
	bestLeftIdx := []int(nil)
	bestRightIdx := []int(nil)

	for f := 0; f < dim; f++ {
		// Collect unique values for this feature
		vals := make([]float64, len(indices))
		for i, idx := range indices {
			vals[i] = features[idx][f]
		}
		sort.Float64s(vals)

		// Try midpoints between consecutive unique values (sample up to 20)
		thresholds := uniqueMidpoints(vals, 20)
		for _, thresh := range thresholds {
			var leftIdx, rightIdx []int
			var leftOnes, rightOnes float64
			for _, idx := range indices {
				if features[idx][f] <= thresh {
					leftIdx = append(leftIdx, idx)
					leftOnes += labels[idx]
				} else {
					rightIdx = append(rightIdx, idx)
					rightOnes += labels[idx]
				}
			}
			if len(leftIdx) == 0 || len(rightIdx) == 0 {
				continue
			}
			lGini := giniImpurity(leftOnes, float64(len(leftIdx)))
			rGini := giniImpurity(rightOnes, float64(len(rightIdx)))
			weighted := (float64(len(leftIdx))*lGini + float64(len(rightIdx))*rGini) / float64(len(indices))
			if weighted < bestGini {
				bestGini = weighted
				bestFeature = f
				bestThresh = thresh
				bestLeftIdx = leftIdx
				bestRightIdx = rightIdx
			}
		}
	}

	if bestLeftIdx == nil {
		return &treeNode{isLeaf: true, value: majority}
	}

	return &treeNode{
		feature: bestFeature,
		thresh:  bestThresh,
		left:    buildTree(features, labels, bestLeftIdx, maxDepth, minSamples, depth+1),
		right:   buildTree(features, labels, bestRightIdx, maxDepth, minSamples, depth+1),
	}
}

func predictTree(node *treeNode, x []float64) float64 {
	if node.isLeaf {
		return node.value
	}
	if node.feature < len(x) && x[node.feature] <= node.thresh {
		return predictTree(node.left, x)
	}
	return predictTree(node.right, x)
}

func giniImpurity(ones, total float64) float64 {
	if total == 0 {
		return 0
	}
	p := ones / total
	return 1 - p*p - (1-p)*(1-p)
}

func uniqueMidpoints(sorted []float64, maxN int) []float64 {
	var mids []float64
	prev := sorted[0]
	for i := 1; i < len(sorted); i++ {
		if sorted[i] != prev {
			mids = append(mids, (prev+sorted[i])/2.0)
			prev = sorted[i]
		}
	}
	if len(mids) <= maxN {
		return mids
	}
	// Sample evenly
	step := float64(len(mids)) / float64(maxN)
	sampled := make([]float64, maxN)
	for i := 0; i < maxN; i++ {
		sampled[i] = mids[int(float64(i)*step)]
	}
	return sampled
}

// -----------------------------------------------------------------------
// Feature scaling utilities
// -----------------------------------------------------------------------

func normalizeMatrix(features [][]float64) []interface{} {
	n := len(features)
	dim := len(features[0])

	mins := make([]float64, dim)
	maxs := make([]float64, dim)
	for j := 0; j < dim; j++ {
		mins[j] = math.Inf(1)
		maxs[j] = math.Inf(-1)
	}
	for i := 0; i < n; i++ {
		for j := 0; j < dim && j < len(features[i]); j++ {
			if features[i][j] < mins[j] {
				mins[j] = features[i][j]
			}
			if features[i][j] > maxs[j] {
				maxs[j] = features[i][j]
			}
		}
	}

	out := make([]interface{}, n)
	for i := 0; i < n; i++ {
		row := make([]float64, dim)
		for j := 0; j < dim && j < len(features[i]); j++ {
			rng := maxs[j] - mins[j]
			if rng == 0 {
				row[j] = 0
			} else {
				row[j] = (features[i][j] - mins[j]) / rng
			}
		}
		out[i] = row
	}
	return out
}

func standardizeMatrix(features [][]float64) []interface{} {
	n := len(features)
	dim := len(features[0])

	means := make([]float64, dim)
	for i := 0; i < n; i++ {
		for j := 0; j < dim && j < len(features[i]); j++ {
			means[j] += features[i][j]
		}
	}
	for j := 0; j < dim; j++ {
		means[j] /= float64(n)
	}

	stds := make([]float64, dim)
	for i := 0; i < n; i++ {
		for j := 0; j < dim && j < len(features[i]); j++ {
			d := features[i][j] - means[j]
			stds[j] += d * d
		}
	}
	for j := 0; j < dim; j++ {
		stds[j] = math.Sqrt(stds[j] / float64(n))
		if stds[j] == 0 {
			stds[j] = 1
		}
	}

	out := make([]interface{}, n)
	for i := 0; i < n; i++ {
		row := make([]float64, dim)
		for j := 0; j < dim && j < len(features[i]); j++ {
			row[j] = (features[i][j] - means[j]) / stds[j]
		}
		out[i] = row
	}
	return out
}
