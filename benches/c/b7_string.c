/* B7 字符串核 · 重复切片拼接 + 查找（C 基线，-O2）。
 * 与 benches/b7_string.aic 逐行同构：str = {ptr,len} 视图，零拷贝切片。 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct { const char *p; size_t len; } str;

static long checksum(str s) {
    long sum = 0;
    for (size_t i = 0; i < s.len; i++) sum += (long)(unsigned char)s.p[i];
    return sum;
}

static long index_of(str hay, const char *needle) {
    size_t n = strlen(needle);
    if (n == 0) return 0;
    if (n > hay.len) return -1;
    for (size_t i = 0; i + n <= hay.len; i++) {
        if (memcmp(hay.p + i, needle, n) == 0) return (long)i;
    }
    return -1;
}

int main(int argc, char **argv) {
    long rounds = 2000;
    if (argc >= 2) rounds = atol(argv[1]);
    str base = {"the quick brown fox jumps over the lazy dog 0123456789", 54};
    long acc = 0;
    for (long r = 0; r < rounds; r++) {
        str v = {base.p, 20};
        acc += checksum(v);
        long pos = index_of(base, "fox");
        if (pos >= 0) acc += pos;
        str j = {base.p + 10, 20};
        acc += checksum(j);
    }
    printf("%ld\n", acc);
    printf("%zu\n", base.len);
    return 0;
}
