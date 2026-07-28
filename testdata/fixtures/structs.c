#include <stdio.h>
#include <stdlib.h>
#include <string.h>

struct item {
	int id;
	char name[16];
	double weight;
};

struct config {
	int count;
	struct item *items;
	const char *label;
	int matrix[2][3];
};

static void inspect(struct config *cfg)
{
	char buf[64];

	snprintf(buf, sizeof buf, "label=%s count=%d", cfg->label, cfg->count);
	printf("%s\n", buf);
}

int main(void)
{
	struct config cfg;
	int i;

	cfg.count = 3;
	cfg.label = "demo";
	cfg.items = calloc(cfg.count, sizeof *cfg.items);
	for (i = 0; i < cfg.count; i++) {
		cfg.items[i].id = i;
		snprintf(cfg.items[i].name, sizeof cfg.items[i].name, "item-%d", i);
		cfg.items[i].weight = i * 1.5;
	}
	memset(cfg.matrix, 0, sizeof cfg.matrix);
	cfg.matrix[1][2] = 7;

	inspect(&cfg);
	free(cfg.items);
	return 0;
}
