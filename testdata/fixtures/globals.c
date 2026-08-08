#include <stdio.h>
#include <string.h>

/*
 * The fixture the documentation screenshots are taken from.
 *
 * Everything else in this directory keeps its state on the stack, which leaves
 * the memory viewer's symbol column with nothing to name: gdb resolves an
 * address to `<symbol>` only for something the symbol table knows, and the
 * stack and heap are honestly blank. So this one puts named objects at fixed
 * addresses, of enough different kinds to show what the column does.
 *
 * Built -no-pie for the docs, so the addresses printed in the prose are the
 * ones a reader will see.
 */

int    counter = 7;
char   banner[32] = "gdb-wui";
double ratios[4] = {1.5, 2.5, 3.5, 4.5};

/* Static, to show that the column names what only the symbol table knows. */
static long hidden_total = 0x4142434445464748L;

struct node {
	int id;
	const char *name;
	struct node *next;
};

struct node tail = { 2, "tail", 0 };
struct node head = { 1, "head", &tail };

/* A pointer per kind, for "show what it points to" from the right-click menu. */
void *pointers[3] = { &counter, banner, &head };

struct summary {
	int visited;
	long total;
	const char *last;
};

static void walk(struct summary *out)
{
	struct node *n = &head;

	out->visited = 0;
	out->total = 0;
	while (n != 0) {
		out->visited++;
		out->total += n->id;
		out->last = n->name;
		n = n->next;
	}
}

int main(void)
{
	struct summary s;
	int i;

	for (i = 0; i < 4; i++)
		hidden_total += (long)(ratios[i] * counter);

	walk(&s);
	printf("%s visited=%d total=%ld last=%s hidden=%lx\n",
	       banner, s.visited, s.total, s.last, hidden_total);
	return 0;
}
