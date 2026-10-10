/* B6 递归核 · fib(naive)（C 基线，-O2）。与 benches/b6_fib.aic 逐行同构。 */
#include <stdio.h>
#include <stdlib.h>

static long fib(long n) {
    if (n < 2) return n;
    return fib(n - 1) + fib(n - 2);
}

int main(int argc, char **argv) {
    long n = 42;
    if (argc >= 2) n = atol(argv[1]);
    printf("%ld\n", fib(n));
    return 0;
}
