//go:build linux

package boot

import "testing"

func TestAddGrubCmdlineParam(t *testing.T) {
	tests := []struct {
		name    string
		content string
		param   string
		want    string
		wantOK  bool
	}{
		{
			name:    "standard grub config",
			content: "GRUB_DEFAULT=0\nGRUB_CMDLINE_LINUX=\"console=ttyS0\"\nGRUB_TIMEOUT=5\n",
			param:   "no5lvl",
			want:    "GRUB_DEFAULT=0\nGRUB_CMDLINE_LINUX=\"console=ttyS0 no5lvl\"\nGRUB_TIMEOUT=5\n",
			wantOK:  true,
		},
		{
			name:    "empty cmdline",
			content: "GRUB_CMDLINE_LINUX=\"\"\n",
			param:   "no5lvl",
			want:    "GRUB_CMDLINE_LINUX=\" no5lvl\"\n",
			wantOK:  true,
		},
		{
			name:    "multiple params",
			content: "GRUB_CMDLINE_LINUX=\"console=tty1 console=ttyS0 quiet\"\n",
			param:   "no5lvl",
			want:    "GRUB_CMDLINE_LINUX=\"console=tty1 console=ttyS0 quiet no5lvl\"\n",
			wantOK:  true,
		},
		{
			name:    "no GRUB_CMDLINE_LINUX",
			content: "GRUB_DEFAULT=0\nGRUB_TIMEOUT=5\n",
			param:   "no5lvl",
			want:    "GRUB_DEFAULT=0\nGRUB_TIMEOUT=5\n",
			wantOK:  false,
		},
		{
			name:    "GRUB_CMDLINE_LINUX_DEFAULT not touched",
			content: "GRUB_CMDLINE_LINUX_DEFAULT=\"quiet splash\"\nGRUB_CMDLINE_LINUX=\"console=ttyS0\"\n",
			param:   "no5lvl",
			want:    "GRUB_CMDLINE_LINUX_DEFAULT=\"quiet splash\"\nGRUB_CMDLINE_LINUX=\"console=ttyS0 no5lvl\"\n",
			wantOK:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := addGrubCmdlineParam(tt.content, tt.param)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}
