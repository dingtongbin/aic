/* B1 数值核 · 素数筛（C 基线，-O2）。
 * 与 benches/b1_sieve.aic **逐行同构**：同一个 bool 数组、同样的标记次序。 */
#include <stdio.h>
#include <stdlib.h>
#include <stdbool.h>

static void sieve(long limit, bool *composite) {
    for (long p = 2; p * p <= limit; p++) {
        if (!composite[p]) {
            for (long m = p * p; m <= limit; m += p) composite[m] = true;
        }
    }
}

int main(int argc, char **argv) {
    long limit = 100000000;
    if (argc >= 2) limit = atol(argv[1]);
    bool *composite = calloc((size_t)limit + 1, sizeof(bool));
    sieve(limit, composite);
    long count = 0;
    for (long i = 2; i <= limit; i++) if (!composite[i]) count++;
    printf("%ld\n", count);
    free(composite);
    return 0;
}
