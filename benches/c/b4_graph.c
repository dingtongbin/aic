/* B4 分配：百万节点对象图（C 基线，-O2）。
 * 与 benches/b4_graph.aic **同构**：同样的链表、同样的构建/遍历次序、同样的校验和；
 * 差别只在回收方式（C 基线 = 逐节点 free；AIC = region 弹出整块回收）。 */
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>

typedef struct Node {
    int64_t value;
    struct Node *next;
} Node;

static Node *build(int64_t n) {
    Node *head = NULL;
    for (int64_t i = 0; i < n; i++) {
        Node *node = malloc(sizeof(Node));
        node->value = i;
        node->next = head;
        head = node;
    }
    return head;
}

static int64_t traverse(Node *head) {
    int64_t sum = 0;
    for (Node *cur = head; cur != NULL; cur = cur->next) sum += cur->value;
    return sum;
}

int main(int argc, char **argv) {
    int64_t n = 1000000;
    if (argc >= 2) n = atoll(argv[1]);
    Node *head = build(n);
    int64_t checksum = traverse(head);
    printf("%lld\n", (long long)checksum);
    while (head != NULL) {
        Node *next = head->next;
        free(head);
        head = next;
    }
    return 0;
}
