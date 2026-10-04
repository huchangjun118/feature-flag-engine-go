package flagengine

import (
	"sync"
	"testing"
)

func TestHashBucketDeterministic(t *testing.T) {
	key := bucketKey("salt", "feature_x", "user-42")
	first := hashBucket(key)
	for i := 0; i < 1000; i++ {
		if got := hashBucket(key); got != first {
			t.Fatalf("bucket changed across calls: %d vs %d", got, first)
		}
	}
	if first < 0 || first >= BucketCount {
		t.Fatalf("bucket out of range: %d", first)
	}
}

func TestBucketStableAcrossProcessRestart(t *testing.T) {
	// 模拟重启：重新构造 Flag 定义（等价定义），桶值必须不变。
	f1 := &Flag{Key: "checkout_new", Rules: []Rule{{ID: "r1", Percentage: 10}}}
	f2 := &Flag{Key: "checkout_new", Rules: []Rule{{ID: "r1", Percentage: 90}}}
	for _, subj := range []string{"a", "b", "c", "user-1", "用户#9"} {
		b1 := computeBucket(f1, subj)
		b2 := computeBucket(f2, subj)
		if b1 != b2 {
			t.Fatalf("bucket moved when percentage changed for %q: %d -> %d", subj, b1, b2)
		}
	}
}

func TestBucketConcurrentStable(t *testing.T) {
	flag := &Flag{Key: "k"}
	want := computeBucket(flag, "user-7")
	var wg sync.WaitGroup
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				if computeBucket(flag, "user-7") != want {
					t.Errorf("concurrent bucket drift")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestInBucketMonotonic(t *testing.T) {
	// 比例上调时，任何主体不会从"已放量"变成"未放量"（以其固定桶位论）。
	for b := 0; b < BucketCount; b++ {
		wasIn := false
		for p := 0; p <= 100; p++ {
			now := inBucket(b, p)
			if wasIn && !now {
				t.Fatalf("bucket %d kicked out while raising percentage to %d", b, p)
			}
			wasIn = now
		}
	}
	if inBucket(0, 0) {
		t.Fatal("percentage 0 must keep everyone out")
	}
	for b := 0; b < BucketCount; b++ {
		if !inBucket(b, 100) {
			t.Fatalf("percentage 100 must include bucket %d", b)
		}
	}
}

func TestBucketDistributionRoughlyUniform(t *testing.T) {
	flag := &Flag{Key: "dist"}
	counts := make([]int, BucketCount)
	const n = 100_000
	for i := 0; i < n; i++ {
		counts[computeBucket(flag, intToSubject(i))]++
	}
	// 每个桶期望 1000，允许较宽偏差（此测试主要发现系统性塌缩，而非做统计检验）。
	for b, c := range counts {
		if c < n/BucketCount/2 || c > n/BucketCount*2 {
			t.Fatalf("bucket %d count %d looks non-uniform", b, c)
		}
	}
}

func intToSubject(i int) string {
	// 不引入 strconv：手写简单转换
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
