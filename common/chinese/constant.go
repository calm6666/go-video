package chinese

// 内置 OpenCC 方案配置，使用 JSON 描述字典链。
// 每个 conversion_chain 项的 dict 字段可以是 group（嵌套一组 dicts）或 txt（单个字典文件）。
// 字典文件按行存储：key<TAB>value1[<TAB>value2...]。
var (
	hk2s = `{
		"name": "Traditional Chinese (Hong Kong standard) to Simplified Chinese",
		"segmentation": {
			"type": "mmseg",
			"dict": {
				"type": "txt",
				"file": "TSPhrases.txt"
			}
		},
		"conversion_chain": [{
			"dict": {
				"type": "group",
				"dicts": [{
					"type": "txt",
					"file": "HKVariantsRevPhrases.txt"
				}, {
					"type": "txt",
					"file": "HKVariantsRev.txt"
				}]
			}
		}, {
			"dict": {
				"type": "group",
				"dicts": [{
					"type": "txt",
					"file": "TSPhrases.txt"
				}, {
					"type": "txt",
					"file": "TSCharacters.txt"
				}]
			}
		}]
	}
	`
	s2hk = `{
		"name": "Simplified Chinese to Traditional Chinese (Hong Kong standard)",
		"segmentation": {
			"type": "mmseg",
			"dict": {
				"type": "txt",
				"file": "STPhrases.txt"
			}
		},
		"conversion_chain": [{
			"dict": {
				"type": "group",
				"dicts": [{
					"type": "txt",
					"file": "STPhrases.txt"
				}, {
					"type": "txt",
					"file": "STCharacters.txt"
				}]
			}
		}, {
			"dict": {
				"type": "group",
				"dicts": [{
					"type": "txt",
					"file": "HKVariantsPhrases.txt"
				}, {
					"type": "txt",
					"file": "HKVariants.txt"
				}]
			}
		}]
	}
	`
	s2t = `{
		"name": "Simplified Chinese to Traditional Chinese",
		"segmentation": {
			"type": "mmseg",
			"dict": {
				"type": "txt",
				"file": "STPhrases.txt"
			}
		},
		"conversion_chain": [{
			"dict": {
				"type": "group",
				"dicts": [{
					"type": "txt",
					"file": "STPhrases.txt"
				}, {
					"type": "txt",
					"file": "STCharacters.txt"
				}]
			}
		}]
	}
	`
	s2tw = `{
		"name": "Simplified Chinese to Traditional Chinese (Taiwan standard)",
		"segmentation": {
			"type": "mmseg",
			"dict": {
				"type": "txt",
				"file": "STPhrases.txt"
			}
		},
		"conversion_chain": [{
			"dict": {
				"type": "group",
				"dicts": [{
					"type": "txt",
					"file": "STPhrases.txt"
				}, {
					"type": "txt",
					"file": "STCharacters.txt"
				}]
			}
		}, {
			"dict": {
				"type": "txt",
				"file": "TWVariants.txt"
			}
		}]
	}
	`
	s2twp = `{
		"name": "Simplified Chinese to Traditional Chinese (Taiwan standard, with phrases)",
		"segmentation": {
			"type": "mmseg",
			"dict": {
				"type": "txt",
				"file": "STPhrases.txt"
			}
		},
		"conversion_chain": [{
			"dict": {
				"type": "group",
				"dicts": [{
					"type": "txt",
					"file": "STPhrases.txt"
				}, {
					"type": "txt",
					"file": "STCharacters.txt"
				}]
			}
		}, {
			"dict": {
				"type": "txt",
				"file": "TWPhrases.txt"
			}
		}, {
			"dict": {
				"type": "txt",
				"file": "TWVariants.txt"
			}
		}]
	}
	`
	t2hk = `{
		"name": "Traditional Chinese to Traditional Chinese (Hong Kong standard)",
		"segmentation": {
			"type": "mmseg",
			"dict": {
				"type": "txt",
				"file": "HKVariants.txt"
			}
		},
		"conversion_chain": [{
			"dict": {
				"type": "txt",
				"file": "HKVariants.txt"
			}
		}]
	}
	`
	t2s = `{
		"name": "Traditional Chinese to Simplified Chinese",
		"segmentation": {
			"type": "mmseg",
			"dict": {
				"type": "txt",
				"file": "TSPhrases.txt"
			}
		},
		"conversion_chain": [{
			"dict": {
				"type": "group",
				"dicts": [{
					"type": "txt",
					"file": "TSPhrases.txt"
				}, {
					"type": "txt",
					"file": "TSCharacters.txt"
				}]
			}
		}]
	}
	`
	t2tw = `{
		"name": "Traditional Chinese to Traditional Chinese (Taiwan standard)",
		"segmentation": {
			"type": "mmseg",
			"dict": {
				"type": "txt",
				"file": "TWVariants.txt"
			}
		},
		"conversion_chain": [{
			"dict": {
				"type": "txt",
				"file": "TWVariants.txt"
			}
		}]
	}
	`
	tw2s = `{
		"name": "Traditional Chinese (Taiwan standard) to Simplified Chinese",
		"segmentation": {
			"type": "mmseg",
			"dict": {
				"type": "txt",
				"file": "TSPhrases.txt"
			}
		},
		"conversion_chain": [{
			"dict": {
				"type": "group",
				"dicts": [{
					"type": "txt",
					"file": "TWVariantsRevPhrases.txt"
				}, {
					"type": "txt",
					"file": "TWVariantsRev.txt"
				}]
			}
		}, {
			"dict": {
				"type": "group",
				"dicts": [{
					"type": "txt",
					"file": "TSPhrases.txt"
				}, {
					"type": "txt",
					"file": "TSCharacters.txt"
				}]
			}
		}]
	}
	`
	tw2sp = `{
		"name": "Traditional Chinese (Taiwan standard) to Simplified Chinese (with phrases)",
		"segmentation": {
			"type": "mmseg",
			"dict": {
				"type": "txt",
				"file": "TSPhrases.txt"
			}
		},
		"conversion_chain": [{
			"dict": {
				"type": "group",
				"dicts": [{
					"type": "txt",
					"file": "TWVariantsRevPhrases.txt"
				}, {
					"type": "txt",
					"file": "TWVariantsRev.txt"
				}]
			}
		}, {
			"dict": {
				"type": "txt",
				"file": "TWPhrasesRev.txt"
			}
		}, {
			"dict": {
				"type": "group",
				"dicts": [{
					"type": "txt",
					"file": "TSPhrases.txt"
				}, {
					"type": "txt",
					"file": "TSCharacters.txt"
				}]
			}
		}]
	}
	`
)

// schemes 内置方案名到 JSON 配置的映射。
var schemes = map[string]string{
	"hk2s":  hk2s,
	"s2hk":  s2hk,
	"s2t":   s2t,
	"s2tw":  s2tw,
	"s2twp": s2twp,
	"t2hk":  t2hk,
	"t2s":   t2s,
	"t2tw":  t2tw,
	"tw2s":  tw2s,
	"tw2sp": tw2sp,
}
