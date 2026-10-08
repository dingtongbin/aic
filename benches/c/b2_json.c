/* B2 解析：JSON 子集递归下降解析器（C 基线，-O2）。
 * 与 benches/b2_json.aic **逐行同构**：同一文法、同一游标推进次序、同一校验和
 * （数字之和 + 键的个数）、同一输入生成方式（约 1MB）。 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

typedef struct {
    const char *src;
    size_t len;
    size_t pos;
} P;

static int64_t parse_value(P *p);

static void skip_ws(P *p) {
    while (p->pos < p->len) {
        char c = p->src[p->pos];
        if (c == ' ' || c == '\n' || c == '\t' || c == '\r') p->pos++;
        else break;
    }
}

static int64_t parse_string(P *p) {
    p->pos++;
    while (p->pos < p->len) {
        char c = p->src[p->pos];
        p->pos++;
        if (c == '"') break;
    }
    return 0;
}

static int64_t parse_number(P *p) {
    int64_t v = 0;
    while (p->pos < p->len) {
        char c = p->src[p->pos];
        if (c >= '0' && c <= '9') { v = v * 10 + (c - '0'); p->pos++; }
        else break;
    }
    return v;
}

static int64_t parse_array(P *p) {
    p->pos++;
    int64_t sum = 0;
    skip_ws(p);
    if (p->pos < p->len && p->src[p->pos] == ']') { p->pos++; return 0; }
    for (;;) {
        sum += parse_value(p);
        skip_ws(p);
        if (p->pos < p->len && p->src[p->pos] == ',') { p->pos++; continue; }
        break;
    }
    skip_ws(p);
    if (p->pos < p->len && p->src[p->pos] == ']') p->pos++;
    return sum;
}

static int64_t parse_object(P *p) {
    p->pos++;
    int64_t sum = 0, keys = 0;
    skip_ws(p);
    if (p->pos < p->len && p->src[p->pos] == '}') { p->pos++; return 0; }
    for (;;) {
        skip_ws(p);
        parse_string(p);
        keys++;
        skip_ws(p);
        if (p->pos < p->len && p->src[p->pos] == ':') p->pos++;
        sum += parse_value(p);
        skip_ws(p);
        if (p->pos < p->len && p->src[p->pos] == ',') { p->pos++; continue; }
        break;
    }
    skip_ws(p);
    if (p->pos < p->len && p->src[p->pos] == '}') p->pos++;
    return sum + keys;
}

static int64_t parse_value(P *p) {
    skip_ws(p);
    if (p->pos >= p->len) return 0;
    char c = p->src[p->pos];
    if (c == '{') return parse_object(p);
    if (c == '[') return parse_array(p);
    if (c == '"') return parse_string(p);
    if (c == 't') { p->pos += 4; return 0; }
    if (c == 'f') { p->pos += 5; return 0; }
    if (c == 'n') { p->pos += 4; return 0; }
    return parse_number(p);
}

int main(int argc, char **argv) {
    if (argc < 2) { printf("usage: b2_json <file.json>\n"); return 0; }
    FILE *f = fopen(argv[1], "rb");
    if (!f) { printf("cannot open file\n"); return 1; }
    fseek(f, 0, SEEK_END);
    long n = ftell(f);
    fseek(f, 0, SEEK_SET);
    char *doc = malloc((size_t)n + 1);
    size_t got = fread(doc, 1, (size_t)n, f);
    doc[got] = 0;
    fclose(f);

    P p = {doc, got, 0};
    int64_t sum = parse_value(&p);
    printf("%lld\n", (long long)sum);
    printf("%zu\n", got);
    free(doc);
    return 0;
}
